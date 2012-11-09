// Receve an Authenticate message and authenticate the user
package ntlm

import (
	"bytes"
	"errors"
	"ntlm/messages"
	"strings"
)

/*******************************
 Shared Session Data and Methods
*******************************/

type V1Session struct {
	SessionData
}

func (n *V1Session) SetUserInfo(username string, password string, domain string) {
	n.user = username
	n.password = password
	n.userDomain = domain
}

func (n *V1Session) SetMode(mode Mode) {
  n.mode = mode
}

func (n *V1Session) fetchResponseKeys() (err error) {
	n.responseKeyLM, err = lmowfv1(n.password)
	if err != nil {
		return err
	}
	n.responseKeyNT = ntowfv1(n.password)
	return
}

func (n *V1Session) computeExpectedResponses() (err error) {
	if messages.NTLMSSP_NEGOTIATE_EXTENDED_SESSIONSECURITY.IsSet(n.negotiateFlags) {
		n.ntChallengeResponse, err = desL(n.responseKeyNT, md5(concat(n.serverChallenge, n.clientChallenge))[0:8])
		if err != nil {
			return err
		}
		n.lmChallengeResponse = concat(n.clientChallenge, make([]byte, 16))
	} else {
		n.ntChallengeResponse, err = desL(n.responseKeyNT, n.serverChallenge)
		if err != nil {
			return err
		}
		// NoLMResponseNTLMv1: A Boolean setting that controls using the NTLM response for the LM
		// response to the server challenge when NTLMv1 authentication is used.<30>
		// <30> Section 3.1.1.1: The default value of this state variable is TRUE. Windows NT Server 4.0 SP3
		// does not support providing NTLM instead of LM responses.
		noLmResponseNtlmV1 := false
		if noLmResponseNtlmV1 {
			n.lmChallengeResponse = n.ntChallengeResponse
		} else {
			n.lmChallengeResponse, err = desL(n.responseKeyLM, n.serverChallenge)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

func (n *V1Session) computeSessionBaseKey() (err error) {
	n.sessionBaseKey = md4(n.responseKeyNT)
	return
}

func (n *V1Session) computeKeyExchangeKey() (err error) {
	if messages.NTLMSSP_NEGOTIATE_EXTENDED_SESSIONSECURITY.IsSet(n.negotiateFlags) {
		n.keyExchangeKey = hmacMd5(n.sessionBaseKey, concat(n.serverChallenge, n.lmChallengeResponse[0:8]))
	} else {
		n.keyExchangeKey, err = kxKey(n.negotiateFlags, n.sessionBaseKey, n.lmChallengeResponse, n.serverChallenge, n.responseKeyLM)
	}
	return
}

func (n *V1Session) calculateKeys() (err error) {
	n.clientSigningKey = signKey(n.negotiateFlags, n.exportedSessionKey, "Client")
	n.serverSigningKey = signKey(n.negotiateFlags, n.exportedSessionKey, "Server")
	n.clientSealingKey = sealKey(n.negotiateFlags, n.exportedSessionKey, "Client")
	n.serverSealingKey = sealKey(n.negotiateFlags, n.exportedSessionKey, "Server")
	return
}

func (n *V1Session) Seal(message []byte) ([]byte, error) {
	return nil, nil
}

func (n *V1Session) Sign(message []byte) ([]byte, error) {
	return nil, nil
}

func (n *V1Session) Mac(message []byte, sequenceNumber int) ([]byte, error) {
	sig := mac(n.negotiateFlags, n.serverHandle, n.serverSigningKey, uint32(sequenceNumber), message)
	return sig.Bytes(), nil
}

/**************
 Server Session
**************/

type V1ServerSession struct {
	V1Session
}

func (n *V1ServerSession) ProcessNegotiateMessage(nm *messages.Negotiate) (err error) {
	n.negotiateMessage = nm
	return
}

func (n *V1ServerSession) GenerateChallengeMessage() (cm *messages.Challenge, err error) {
	// TODO: Generate this challenge message
	return
}

func (n *V1ServerSession) ProcessAuthenticateMessage(am *messages.Authenticate) (err error) {
	n.authenticateMessage = am
	n.negotiateFlags = am.NegotiateFlags
	n.clientChallenge = am.ClientChallenge()
	n.encryptedRandomSessionKey = am.EncryptedRandomSessionKey.Payload

	err = n.fetchResponseKeys()
	if err != nil {
		return err
	}

	err = n.computeExpectedResponses()
	if err != nil {
		return err
	}

	err = n.computeSessionBaseKey()
	if err != nil {
		return err
	}

	err = n.computeKeyExchangeKey()
	if err != nil {
		return err
	}

	if !bytes.Equal(am.NtChallengeResponseFields.Payload, n.ntChallengeResponse) {
		if !bytes.Equal(am.LmChallengeResponse.Payload, n.lmChallengeResponse) {
			return errors.New("Could not authenticate")
		}
	}

	n.mic = am.Mic
	am.Mic = zeroBytes(16)

	err = n.computeExportedSessionKey()
	if err != nil {
		return err
	}

	err = n.calculateKeys()
	if err != nil {
		return err
	}

	n.clientHandle, err = rc4Init(n.clientSealingKey)
	if err != nil {
		return err
	}
	n.serverHandle, err = rc4Init(n.serverSealingKey)
	if err != nil {
		return err
	}

	return nil
}

func (n *V1ServerSession) computeExportedSessionKey() (err error) {
	if messages.NTLMSSP_NEGOTIATE_KEY_EXCH.IsSet(n.negotiateFlags) {
		n.exportedSessionKey, err = rc4K(n.keyExchangeKey, n.encryptedRandomSessionKey)
		if err != nil {
			return err
		}
		// TODO: Calculate mic correctly. This calculation is not producing the right results now
		// n.calculatedMic = HmacMd5(n.exportedSessionKey, concat(n.challengeMessage.Payload, n.authenticateMessage.Bytes))
	} else {
		n.exportedSessionKey = n.keyExchangeKey
		// TODO: Calculate mic correctly. This calculation is not producing the right results now
		// n.calculatedMic = HmacMd5(n.keyExchangeKey, concat(n.challengeMessage.Payload, n.authenticateMessage.Bytes))
	}
	return nil
}

/*************
 Client Session
**************/

type V1ClientSession struct {
	V1Session
}

func (n *V1ClientSession) GenerateNegotiateMessage() (nm *messages.Negotiate, err error) {
	return nil, nil
}

func (n *V1ClientSession) ProcessChallengeMessage(cm *messages.Challenge) (err error) {
	n.challengeMessage = cm
	n.serverChallenge = cm.ServerChallenge
	n.clientChallenge = randomBytes(8)

	// Set up the default flags for processing the response. These are the flags that we will return
	// in the authenticate message
	flags := uint32(0)
	flags = messages.NTLMSSP_NEGOTIATE_KEY_EXCH.Set(flags)
	// NOTE: Unsetting this flag in order to get the server to generate the signatures we can recognize
  // flags = messages.NTLMSSP_NEGOTIATE_VERSION.Set(flags)
	flags = messages.NTLMSSP_NEGOTIATE_TARGET_INFO.Set(flags)
	flags = messages.NTLMSSP_NEGOTIATE_IDENTIFY.Set(flags)
	flags = messages.NTLMSSP_NEGOTIATE_ALWAYS_SIGN.Set(flags)
	flags = messages.NTLMSSP_NEGOTIATE_NTLM.Set(flags)
	flags = messages.NTLMSSP_NEGOTIATE_DATAGRAM.Set(flags)
	flags = messages.NTLMSSP_NEGOTIATE_SIGN.Set(flags)
	flags = messages.NTLMSSP_REQUEST_TARGET.Set(flags)
	flags = messages.NTLMSSP_NEGOTIATE_UNICODE.Set(flags)

	n.negotiateFlags = flags

	err = n.fetchResponseKeys()
	if err != nil {
		return err
	}

	err = n.computeExpectedResponses()
	if err != nil {
		return err
	}

	err = n.computeSessionBaseKey()
	if err != nil {
		return err
	}

	err = n.computeKeyExchangeKey()
	if err != nil {
		return err
	}

	err = n.computeEncryptedSessionKey()
	if err != nil {
		return err
	}

	err = n.calculateKeys()
	if err != nil {
		return err
	}

	n.clientHandle, err = rc4Init(n.clientSealingKey)
	if err != nil {
		return err
	}
	n.serverHandle, err = rc4Init(n.serverSealingKey)
	if err != nil {
		return err
	}

	return nil
}

func (n *V1ClientSession) GenerateAuthenticateMessage() (am *messages.Authenticate, err error) {
	am = new(messages.Authenticate)
	am.Signature = []byte("NTLMSSP\x00")
	am.MessageType = uint32(3)
	am.LmChallengeResponse, _ = messages.CreateBytePayload(n.lmChallengeResponse)
	am.NtChallengeResponseFields, _ = messages.CreateBytePayload(n.ntChallengeResponse)
	am.DomainName, _ = messages.CreateStringPayload(n.userDomain)
	am.UserName, _ = messages.CreateStringPayload(n.user)
	am.Workstation, _ = messages.CreateStringPayload("SQUAREMILL")
	am.EncryptedRandomSessionKey, _ = messages.CreateBytePayload(n.encryptedRandomSessionKey)
	am.NegotiateFlags = n.negotiateFlags
	am.Version = &messages.VersionStruct{ProductMajorVersion: uint8(5), ProductMinorVersion: uint8(1), ProductBuild: uint16(2600), NTLMRevisionCurrent: uint8(15)}
	return am, nil
}

func (n *V1ClientSession) computeEncryptedSessionKey() (err error) {
	if messages.NTLMSSP_NEGOTIATE_KEY_EXCH.IsSet(n.negotiateFlags) {
		n.exportedSessionKey = randomBytes(16)
		n.encryptedRandomSessionKey, err = rc4K(n.keyExchangeKey, n.exportedSessionKey)
		if err != nil {
			return err
		}
	} else {
		n.encryptedRandomSessionKey = n.keyExchangeKey
	}
	return nil
}

/********************************
 NTLM V1 Password hash functions
*********************************/

func ntowfv1(passwd string) []byte {
	return md4(utf16FromString(passwd))
}

//	ConcatenationOf( DES( UpperCase( Passwd)[0..6],"KGS!@#$%"), DES( UpperCase( Passwd)[7..13],"KGS!@#$%"))
func lmowfv1(passwd string) ([]byte, error) {
	asciiPassword := []byte(strings.ToUpper(passwd))
	keyBytes := zeroPaddedBytes(asciiPassword, 0, 14)

	first, err := des(keyBytes[0:7], []byte("KGS!@#$%"))
	if err != nil {
		return nil, err
	}
	second, err := des(keyBytes[7:14], []byte("KGS!@#$%"))
	if err != nil {
		return nil, err
	}

	return append(first, second...), nil
}