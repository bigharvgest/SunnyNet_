// Copyright 2018 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package tls

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdh"
	"crypto/hmac"
	"crypto/rsa"
	"errors"
	"fmt"
	"hash"
	"time"

	"github.com/qtgolang/SunnyNet/src/tlsClient/circl/kem"
)

// [uTLS SECTION START]
// KeySharesParameters serves as a in-memory storage for generated keypairs by UTLS when generating
// ClientHello. It is used to store both ecdhe and kem keypairs.
type KeySharesParameters struct {
	ecdhePrivKeymap map[CurveID]*ecdh.PrivateKey
	ecdhePubKeymap  map[CurveID]*ecdh.PublicKey

	// based on cloudflare/go
	kemPrivKeymap map[CurveID]kem.PrivateKey
	kemPubKeymap  map[CurveID]kem.PublicKey
}

func NewKeySharesParameters() *KeySharesParameters {
	return &KeySharesParameters{
		ecdhePrivKeymap: make(map[CurveID]*ecdh.PrivateKey),
		ecdhePubKeymap:  make(map[CurveID]*ecdh.PublicKey),

		kemPrivKeymap: make(map[CurveID]kem.PrivateKey),
		kemPubKeymap:  make(map[CurveID]kem.PublicKey),
	}
}

func (ksp *KeySharesParameters) AddEcdheKeypair(curveID CurveID, ecdheKey *ecdh.PrivateKey, ecdhePubKey *ecdh.PublicKey) {
	ksp.ecdhePrivKeymap[curveID] = ecdheKey
	ksp.ecdhePubKeymap[curveID] = ecdhePubKey
}

func (ksp *KeySharesParameters) GetEcdheKey(curveID CurveID) (ecdheKey *ecdh.PrivateKey, ok bool) {
	ecdheKey, ok = ksp.ecdhePrivKeymap[curveID]
	return
}

func (ksp *KeySharesParameters) GetEcdhePubkey(curveID CurveID) (params *ecdh.PublicKey, ok bool) {
	params, ok = ksp.ecdhePubKeymap[curveID]
	return
}

func (ksp *KeySharesParameters) AddKemKeypair(curveID CurveID, kemKey kem.PrivateKey, kemPubKey kem.PublicKey) {
	if curveIdToCirclScheme(curveID) != nil { // only store for circl schemes
		ksp.kemPrivKeymap[curveID] = kemKey
		ksp.kemPubKeymap[curveID] = kemPubKey
	}
}

func (ksp *KeySharesParameters) GetKemKey(curveID CurveID) (kemKey kem.PrivateKey, ok bool) {
	kemKey, ok = ksp.kemPrivKeymap[curveID]
	return
}

func (ksp *KeySharesParameters) GetKemPubkey(curveID CurveID) (params kem.PublicKey, ok bool) {
	params, ok = ksp.kemPubKeymap[curveID]
	return
}

// [uTLS SECTION END]

type clientHandshakeStateTLS13 struct {
	c               *Conn
	ctx             context.Context
	serverHello     *serverHelloMsg
	hello           *ClientHelloMsg
	ecdheKey        *ecdh.PrivateKey
	kemKey          *kemPrivateKey       // [uTLS] ported from cloudflare/go
	keySharesParams *KeySharesParameters // [uTLS] support both ecdhe and kem

	session       *SessionState
	earlySecret   []byte
	binderKey     []byte
	selectedGroup CurveID

	certReq       *certificateRequestMsgTLS13
	usingPSK      bool
	sentDummyCCS  bool
	suite         *cipherSuiteTLS13
	transcript    hash.Hash
	masterSecret  []byte
	trafficSecret []byte // client_application_traffic_secret_0

	uconn *UConn // [uTLS]
}

// handshake requires hs.c, hs.hello, hs.serverHello, hs.ecdheKey, and,
// optionally, hs.session, hs.earlySecret and hs.binderKey to be set.
func (hs *clientHandshakeStateTLS13) handshake() error {
	c := hs.c

	if needFIPS() {
		return errors.New("tls: internal error: TLS 1.3 reached in FIPS mode")
	}

	// The server must not select TLS 1.3 in a renegotiation. See RFC 8446,
	// sections 4.1.2 and 4.1.3.
	if c.handshakes > 0 {
		c.sendAlert(alertProtocolVersion)
		return errors.New("tls: server selected TLS 1.3 in a renegotiation")
	}

	// [uTLS SECTION START]
	// set echdheParams to what we received from server
	if ecdheKey, ok := hs.keySharesParams.GetEcdheKey(hs.serverHello.serverShare.group); ok {
		hs.ecdheKey = ecdheKey
		hs.kemKey = nil // unset kemKey if any
	}
	// set kemParams to what we received from server
	if kemKey, ok := hs.keySharesParams.GetKemKey(hs.serverHello.serverShare.group); ok {
		hs.kemKey = &kemPrivateKey{
			secretKey: kemKey,
			curveID:   hs.serverHello.serverShare.group,
		}
		hs.ecdheKey = nil // unset ecdheKey if any
	}
	// [uTLS SECTION END]

	// Consistency check on the presence of a keyShare and its parameters.
	if (hs.ecdheKey == nil && hs.kemKey == nil) || len(hs.hello.keyShares) < 1 { // [uTLS]
		// keyshares "< 1" instead of "!= 1", as uTLS may send multiple
		return c.sendAlert(alertInternalError)
	}

	if err := hs.checkServerHelloOrHRR(); err != nil {
		return err
	}

	hs.transcript = hs.suite.hash.New()

	if err := transcriptMsg(hs.hello, hs.transcript); err != nil {
		return err
	}

	if bytes.Equal(hs.serverHello.random, helloRetryRequestRandom) {
		if err := hs.sendDummyChangeCipherSpec(); err != nil {
			return err
		}
		if err := hs.processHelloRetryRequest(); err != nil {
			return err
		}
	}

	if err := transcriptMsg(hs.serverHello, hs.transcript); err != nil {
		return err
	}

	c.buffering = true
	if err := hs.processServerHello(); err != nil {
		return err
	}
	if err := hs.sendDummyChangeCipherSpec(); err != nil {
		return err
	}
	if err := hs.establishHandshakeKeys(); err != nil {
		return err
	}
	if err := hs.readServerParameters(); err != nil {
		return err
	}
	if err := hs.readServerCertificate(); err != nil {
		return err
	}
	if err := hs.readServerFinished(); err != nil {
		return err
	}
	// [UTLS SECTION START]
	if err := hs.serverFinishedReceived(); err != nil {
		return err
	}
	// [UTLS SECTION END]
	if err := hs.sendClientCertificate(); err != nil {
		return err
	}
	if err := hs.sendClientFinished(); err != nil {
		return err
	}
	if _, err := c.flush(); err != nil {
		return err
	}

	c.isHandshakeComplete.Store(true)

	return nil
}

// checkServerHelloOrHRR does validity checks that apply to both ServerHello and
// HelloRetryRequest messages. It sets hs.suite.
func (hs *clientHandshakeStateTLS13) checkServerHelloOrHRR() error {
	c := hs.c

	if hs.serverHello.supportedVersion == 0 {
		c.sendAlert(alertMissingExtension)
		return errors.New("tls: server selected TLS 1.3 using the legacy version field")
	}

	if hs.serverHello.supportedVersion != VersionTLS13 {
		c.sendAlert(alertIllegalParameter)
		return errors.New("tls: server selected an invalid version after a HelloRetryRequest")
	}

	if hs.serverHello.vers != VersionTLS12 {
		c.sendAlert(alertIllegalParameter)
		return errors.New("tls: server sent an incorrect legacy version")
	}

	if hs.serverHello.ocspStapling ||
		hs.serverHello.ticketSupported ||
		hs.serverHello.extendedMasterSecret ||
		hs.serverHello.secureRenegotiationSupported ||
		len(hs.serverHello.secureRenegotiation) != 0 ||
		len(hs.serverHello.alpnProtocol) != 0 ||
		len(hs.serverHello.scts) != 0 {
		c.sendAlert(alertUnsupportedExtension)
		return errors.New("tls: server sent a ServerHello extension forbidden in TLS 1.3")
	}

	if !bytes.Equal(hs.hello.sessionId, hs.serverHello.sessionId) {
		c.sendAlert(alertIllegalParameter)
		return errors.New("tls: server did not echo the legacy session ID")
	}

	if hs.serverHello.compressionMethod != CompressionNone {
		c.sendAlert(alertIllegalParameter)
		return errors.New("tls: server selected unsupported compression format")
	}

	selectedSuite := mutualCipherSuiteTLS13(hs.hello.cipherSuites, hs.serverHello.cipherSuite)
	if hs.suite != nil && selectedSuite != hs.suite {
		c.sendAlert(alertIllegalParameter)
		return errors.New("tls: server changed cipher suite after a HelloRetryRequest")
	}
	if selectedSuite == nil {
		c.sendAlert(alertIllegalParameter)
		return errors.New("tls: server chose an unconfigured cipher suite")
	}
	hs.suite = selectedSuite
	c.cipherSuite = hs.suite.id

	return nil
}

// sendDummyChangeCipherSpec sends a ChangeCipherSpec record for compatibility
// with middleboxes that didn't implement TLS correctly. See RFC 8446, Appendix D.4.
func (hs *clientHandshakeStateTLS13) sendDummyChangeCipherSpec() error {
	if hs.c.quic != nil {
		return nil
	}
	if hs.sentDummyCCS {
		return nil
	}
	hs.sentDummyCCS = true

	return hs.c.writeChangeCipherRecord()
}

// processHelloRetryRequest handles the HRR in hs.serverHello, modifies and
// resends hs.hello, and reads the new ServerHello into hs.serverHello.
func (hs *clientHandshakeStateTLS13) processHelloRetryRequest() error {
	c := hs.c

	// The first ClientHello gets double-hashed into the transcript upon a
	// HelloRetryRequest. (The idea is that the server might offload transcript
	// storage to the client in the cookie.) See RFC 8446, Section 4.4.1.
	chHash := hs.transcript.Sum(nil)
	hs.transcript.Reset()
	hs.transcript.Write([]byte{typeMessageHash, 0, 0, uint8(len(chHash))})
	hs.transcript.Write(chHash)
	if err := transcriptMsg(hs.serverHello, hs.transcript); err != nil {
		return err
	}

	// The only HelloRetryRequest extensions we support are key_share and
	// cookie, and clients must abort the handshake if the HRR would not result
	// in any change in the ClientHello.
	if hs.serverHello.selectedGroup == 0 && hs.serverHello.cookie == nil {
		c.sendAlert(alertIllegalParameter)
		return errors.New("tls: server sent an unnecessary HelloRetryRequest message")
	}

	if hs.serverHello.cookie != nil {
		hs.hello.cookie = hs.serverHello.cookie
	}

	if hs.serverHello.serverShare.group != 0 {
		c.sendAlert(alertDecodeError)
		return errors.New("tls: received malformed key_share extension")
	}

	// If the server sent a key_share extension selecting a group, ensure it's
	// a group we advertised but did not send a key share for, and send a key
	// share for it this time.
	if curveID := hs.serverHello.selectedGroup; curveID != 0 {
		curveOK := false
		for _, id := range hs.hello.supportedCurves {
			if id == curveID {
				curveOK = true
				break
			}
		}
		if !curveOK || !isGroupSupported(curveID) {
			c.sendAlert(alertIllegalParameter)
			return errors.New("tls: server selected unsupported group")
		}

		// [UTLS SECTION BEGINS]
		// ported from cloudflare/go, slightly modified to maintain compatibility with crypto/tls upstream
		if hs.ecdheKey != nil {
			if sentID, _ := curveIDForCurve(hs.ecdheKey.Curve()); sentID == curveID {
				c.sendAlert(alertIllegalParameter)
				return errors.New("tls: server sent an unnecessary HelloRetryRequest key_share")
			}
		} else if hs.kemKey != nil {
			if clientKeySharePrivateCurveID(hs.kemKey) == curveID {
				c.sendAlert(alertIllegalParameter)
				return errors.New("tls: server sent an unnecessary HelloRetryRequest key_share")
			}
		} else {
			c.sendAlert(alertInternalError)
			return errors.New("tls: ecdheKey and kemKey are both nil")
		}

		if scheme := curveIdToCirclScheme(curveID); scheme != nil {
			pk, sk, err := generateKemKeyPair(scheme, curveID, c.config.rand())
			if err != nil {
				c.sendAlert(alertInternalError)
				return fmt.Errorf("HRR generateKemKeyPair %s: %w",
					scheme.Name(), err)
			}
			packedPk, err := pk.MarshalBinary()
			if err != nil {
				c.sendAlert(alertInternalError)
				return fmt.Errorf("HRR pack circl public key %s: %w",
					scheme.Name(), err)
			}
			hs.kemKey = sk
			hs.ecdheKey = nil // unset ecdheKey if any
			hs.hello.keyShares = []keyShare{{group: curveID, data: packedPk}}
		} else {
			if _, ok := curveForCurveID(curveID); !ok {
				c.sendAlert(alertInternalError)
				return errors.New("tls: CurvePreferences includes unsupported curve")
			}
			key, err := generateECDHEKey(c.config.rand(), curveID)
			if err != nil {
				c.sendAlert(alertInternalError)
				return err
			}
			hs.ecdheKey = key
			hs.kemKey = nil // unset kemKey if any
			hs.hello.keyShares = []keyShare{{group: curveID, data: key.PublicKey().Bytes()}}
		}
		// [UTLS SECTION ENDS]
	}

	hs.hello.raw = nil
	if len(hs.hello.pskIdentities) > 0 {
		pskSuite := cipherSuiteTLS13ByID(hs.session.cipherSuite)
		if pskSuite == nil {
			return c.sendAlert(alertInternalError)
		}
		if pskSuite.hash == hs.suite.hash {
			// Update binders and obfuscated_ticket_age.
			ticketAge := c.config.time().Sub(time.Unix(int64(hs.session.createdAt), 0))
			hs.hello.pskIdentities[0].obfuscatedTicketAge = uint32(ticketAge/time.Millisecond) + hs.session.ageAdd

			transcript := hs.suite.hash.New()
			transcript.Write([]byte{typeMessageHash, 0, 0, uint8(len(chHash))})
			transcript.Write(chHash)
			if err := transcriptMsg(hs.serverHello, transcript); err != nil {
				return err
			}
			helloBytes, err := hs.hello.marshalWithoutBinders()
			if err != nil {
				return err
			}
			transcript.Write(helloBytes)
			pskBinders := [][]byte{hs.suite.finishedHash(hs.binderKey, transcript)}
			if err := hs.hello.updateBinders(pskBinders); err != nil {
				return err
			}
		} else {
			// Server selected a cipher suite incompatible with the PSK.
			hs.hello.pskIdentities = nil
			hs.hello.pskBinders = nil
		}
	}

	// [uTLS SECTION BEGINS]
	// crypto/tls code above this point had changed crypto/tls structures in accordance with HRR, and is about
	// to call default marshaller.
	// Instead, we fill uTLS-specific structs and call uTLS marshaller.
	// Only ExtensionCookie, ExtensionPreSharedKey, ExtensionKeyShare, ExtensionEarlyData, ExtensionSupportedVersions,
	// and utlsExtensionPadding are supposed to change
	if hs.uconn != nil {
		if hs.uconn.ClientHelloID.Str() != HelloGolang.Str() {
			if len(hs.hello.pskIdentities) > 0 {
				// TODO: wait for someone who cares about PSK to implement
				return errors.New("uTLS does not support reprocessing of PSK key triggered by HelloRetryRequest")
			}

			keyShareExtFound := false
			for _, ext := range hs.uconn.Extensions {
				// new ks seems to be generated either way
				if ks, ok := ext.(*KeyShareExtension); ok {
					ks.KeyShares = keyShares(hs.hello.keyShares).ToPublic()
					keyShareExtFound = true
				}
			}
			if !keyShareExtFound {
				return errors.New("uTLS: received HelloRetryRequest, but keyshare not found among client's " +
					"uconn.Extensions")
			}

			if len(hs.serverHello.cookie) > 0 {
				// serverHello specified a cookie, let's echo it
				cookieFound := false
				for _, ext := range hs.uconn.Extensions {
					if ks, ok := ext.(*CookieExtension); ok {
						ks.Cookie = hs.serverHello.cookie
						cookieFound = true
					}
				}

				if !cookieFound {
					// pick a random index where to add cookieExtension
					// -2 instead of -1 is a lazy way to ensure that PSK is still a last extension
					p, err := newPRNG()
					if err != nil {
						return err
					}
					cookieIndex := p.Intn(len(hs.uconn.Extensions) - 2)
					if cookieIndex >= len(hs.uconn.Extensions) {
						// this check is for empty hs.uconn.Extensions
						return fmt.Errorf("cookieIndex >= len(hs.uconn.Extensions): %v >= %v",
							cookieIndex, len(hs.uconn.Extensions))
					}
					hs.uconn.Extensions = append(hs.uconn.Extensions[:cookieIndex],
						append([]TLSExtension{&CookieExtension{Cookie: hs.serverHello.cookie}},
							hs.uconn.Extensions[cookieIndex:]...)...)
				}
			}
			if err := hs.uconn.MarshalClientHello(); err != nil {
				return err
			}
			hs.hello.raw = hs.uconn.HandshakeState.Hello.Raw
		}
	}
	// [uTLS SECTION ENDS]
	if hs.hello.earlyData {
		hs.hello.earlyData = false
		c.quicRejectedEarlyData()
	}

	if _, err := hs.c.writeHandshakeRecord(hs.hello, hs.transcript); err != nil {
		return err
	}

	// serverHelloMsg is not included in the transcript
	msg, err := c.readHandshake(nil)
	if err != nil {
		return err
	}

	serverHello, ok := msg.(*serverHelloMsg)
	if !ok {
		c.sendAlert(alertUnexpectedMessage)
		return unexpectedMessageError(serverHello, msg)
	}
	hs.serverHello = serverHello

	if err := hs.checkServerHelloOrHRR(); err != nil {
		return err
	}

	return nil
}

func (hs *clientHandshakeStateTLS13) processServerHello() error {
	c := hs.c

	if bytes.Equal(hs.serverHello.random, helloRetryRequestRandom) {
		c.sendAlert(alertUnexpectedMessage)
		return errors.New("tls: server sent two HelloRetryRequest messages")
	}

	if len(hs.serverHello.cookie) != 0 {
		c.sendAlert(alertUnsupportedExtension)
		return errors.New("tls: server sent a cookie in a normal ServerHello")
	}

	if hs.serverHello.selectedGroup != 0 {
		c.sendAlert(alertDecodeError)
		return errors.New("tls: malformed key_share extension")
	}

	if hs.serverHello.serverShare.group == 0 {
		c.sendAlert(alertIllegalParameter)
		return errors.New("tls: server did not send a key share")
	}

	// [UTLS SECTION BEGINS]
	var supportedGroupCompatible bool
	if hs.ecdheKey != nil { // if we did send ECDHE KeyShare
		if sentID, _ := curveIDForCurve(hs.ecdheKey.Curve()); hs.serverHello.serverShare.group == sentID { // and server selected ECDHE KeyShare
			supportedGroupCompatible = true
		}
	}
	if hs.kemKey != nil && clientKeySharePrivateCurveID(hs.kemKey) == hs.serverHello.serverShare.group { // we did send KEM KeyShare and server selected KEM KeyShare
		supportedGroupCompatible = true
	}
	if !supportedGroupCompatible { // none matched
		c.sendAlert(alertIllegalParameter)
		return errors.New("tls: server selected unsupported group")
	}
	// [UTLS SECTION ENDS]

	if !hs.serverHello.selectedIdentityPresent {
		return nil
	}

	if int(hs.serverHello.selectedIdentity) >= len(hs.hello.pskIdentities) {
		c.sendAlert(alertIllegalParameter)
		return errors.New("tls: server selected an invalid PSK")
	}

	if len(hs.hello.pskIdentities) != 1 || hs.session == nil {
		return c.sendAlert(alertInternalError)
	}
	pskSuite := cipherSuiteTLS13ByID(hs.session.cipherSuite)
	if pskSuite == nil {
		return c.sendAlert(alertInternalError)
	}
	if pskSuite.hash != hs.suite.hash {
		c.sendAlert(alertIllegalParameter)
		return errors.New("tls: server selected an invalid PSK and cipher suite pair")
	}

	hs.usingPSK = true
	c.didResume = true
	c.peerCertificates = hs.session.peerCertificates
	c.activeCertHandles = hs.session.activeCertHandles
	c.verifiedChains = hs.session.verifiedChains
	c.ocspResponse = hs.session.ocspResponse
	c.scts = hs.session.scts
	return nil
}

func (hs *clientHandshakeStateTLS13) establishHandshakeKeys() error {
	c := hs.c

	// [UTLS SECTION BEGINS]
	// ported from cloudflare/go, slightly modified to maintain compatibility with crypto/tls upstream
	var sharedKey []byte
	var err error

	if hs.ecdheKey != nil {
		if ecdheCurveID, _ := curveIDForCurve(hs.ecdheKey.Curve()); ecdheCurveID == hs.serverHello.serverShare.group {
			peerKey, err := hs.ecdheKey.Curve().NewPublicKey(hs.serverHello.serverShare.data)
			if err != nil {
				c.sendAlert(alertIllegalParameter)
				return errors.New("tls: invalid server key share")
			}
			sharedKey, err = hs.ecdheKey.ECDH(peerKey)
			if err != nil {
				c.sendAlert(alertIllegalParameter)
				return errors.New("tls: invalid server key share")
			}
		}
	}
	if sharedKey == nil && hs.kemKey != nil && clientKeySharePrivateCurveID(hs.kemKey) == hs.serverHello.serverShare.group {
		sk := hs.kemKey.secretKey
		sharedKey, err = sk.Scheme().Decapsulate(sk, hs.serverHello.serverShare.data)
		if err != nil {
			c.sendAlert(alertIllegalParameter)
			return fmt.Errorf("%s decaps: %w", sk.Scheme().Name(), err)
		}
	}
	if sharedKey == nil {
		c.sendAlert(alertInternalError)
		return errors.New("tls: ecdheKey and circlKey are both nil")
	}
	// [UTLS SECTION ENDS]

	earlySecret := hs.earlySecret
	if !hs.usingPSK {
		earlySecret = hs.suite.extract(nil, nil)
	}

	handshakeSecret := hs.suite.extract(sharedKey,
		hs.suite.deriveSecret(earlySecret, "derived", nil))

	clientSecret := hs.suite.deriveSecret(handshakeSecret,
		clientHandshakeTrafficLabel, hs.transcript)
	c.out.setTrafficSecret(hs.suite, QUICEncryptionLevelHandshake, clientSecret)
	serverSecret := hs.suite.deriveSecret(handshakeSecret,
		serverHandshakeTrafficLabel, hs.transcript)
	c.in.setTrafficSecret(hs.suite, QUICEncryptionLevelHandshake, serverSecret)

	if c.quic != nil {
		if c.hand.Len() != 0 {
			c.sendAlert(alertUnexpectedMessage)
		}
		c.quicSetWriteSecret(QUICEncryptionLevelHandshake, hs.suite.id, clientSecret)
		c.quicSetReadSecret(QUICEncryptionLevelHandshake, hs.suite.id, serverSecret)
	}

	err = c.config.writeKeyLog(keyLogLabelClientHandshake, hs.hello.random, clientSecret)
	if err != nil {
		c.sendAlert(alertInternalError)
		return err
	}
	err = c.config.writeKeyLog(keyLogLabelServerHandshake, hs.hello.random, serverSecret)
	if err != nil {
		c.sendAlert(alertInternalError)
		return err
	}

	hs.masterSecret = hs.suite.extract(nil,
		hs.suite.deriveSecret(handshakeSecret, "derived", nil))

	return nil
}

func (hs *clientHandshakeStateTLS13) readServerParameters() error {
	c := hs.c

	msg, err := c.readHandshake(hs.transcript)
	if err != nil {
		return err
	}

	encryptedExtensions, ok := msg.(*encryptedExtensionsMsg)
	if !ok {
		c.sendAlert(alertUnexpectedMessage)
		return unexpectedMessageError(encryptedExtensions, msg)
	}

	if err := checkALPN(hs.hello.alpnProtocols, encryptedExtensions.alpnProtocol, c.quic != nil); err != nil {
		// RFC 8446 specifies that no_application_protocol is sent by servers, but
		// does not specify how clients handle the selection of an incompatible protocol.
		// RFC 9001 Section 8.1 specifies that QUIC clients send no_application_protocol
		// in this case. Always sending no_application_protocol seems reasonable.
		c.sendAlert(alertNoApplicationProtocol)
		return err
	}
	c.clientProtocol = encryptedExtensions.alpnProtocol

	// [UTLS SECTION STARTS]
	if hs.uconn != nil {
		err = hs.utlsReadServerParameters(encryptedExtensions)
		if err != nil {
			c.sendAlert(alertUnsupportedExtension)
			return err
		}
	}
	// [UTLS SECTION ENDS]

	if c.quic != nil {
		if encryptedExtensions.quicTransportParameters == nil {
			// RFC 9001 Section 8.2.
			c.sendAlert(alertMissingExtension)
			return errors.New("tls: server did not send a quic_transport_parameters extension")
		}
		c.quicSetTransportParameters(encryptedExtensions.quicTransportParameters)
	} else {
		if encryptedExtensions.quicTransportParameters != nil {
			c.sendAlert(alertUnsupportedExtension)
			return errors.New("tls: server sent an unexpected quic_transport_parameters extension")
		}
	}

	if !hs.hello.earlyData && encryptedExtensions.earlyData {
		c.sendAlert(alertUnsupportedExtension)
		return errors.New("tls: server sent an unexpected early_data extension")
	}
	if hs.hello.earlyData && !encryptedExtensions.earlyData {
		c.quicRejectedEarlyData()
	}
	if encryptedExtensions.earlyData {
		if hs.session.cipherSuite != c.cipherSuite {
			c.sendAlert(alertHandshakeFailure)
			return errors.New("tls: server accepted 0-RTT with the wrong cipher suite")
		}
		if hs.session.alpnProtocol != c.clientProtocol {
			c.sendAlert(alertHandshakeFailure)
			return errors.New("tls: server accepted 0-RTT with the wrong ALPN")
		}
	}

	return nil
}

func (hs *clientHandshakeStateTLS13) readServerCertificate() error {
	c := hs.c

	// Either a PSK or a certificate is always used, but not both.
	// See RFC 8446, Section 4.1.1.
	if hs.usingPSK {
		// Make sure the connection is still being verified whether or not this
		// is a resumption. Resumptions currently don't reverify certificates so
		// they don't call verifyServerCertificate. See Issue 31641.
		if c.config.VerifyConnection != nil {
			if err := c.config.VerifyConnection(c.connectionStateLocked()); err != nil {
				c.sendAlert(alertBadCertificate)
				return err
			}
		}
		return nil
	}

	// [UTLS SECTION BEGINS]
	// msg, err := c.readHandshake(hs.transcript)
	msg, err := c.readHandshake(nil) // hold writing to transcript until we know it is not compressed cert
	// [UTLS SECTION ENDS]
	if err != nil {
		return err
	}

	certReq, ok := msg.(*certificateRequestMsgTLS13)
	if ok {
		hs.certReq = certReq
		transcriptMsg(certReq, hs.transcript) // [UTLS] if it is certReq (not compressedCert), write to transcript

		// msg, err = c.readHandshake(hs.transcript)
		msg, err = c.readHandshake(nil) // [UTLS] we don't write to transcript until make sure it is not compressed cert
		if err != nil {
			return err
		}
	}

	// [UTLS SECTION BEGINS]
	var skipWritingCertToTranscript bool = false
	if hs.uconn != nil {
		processedMsg, err := hs.utlsReadServerCertificate(msg)
		if err != nil {
			return err
		}
		if processedMsg != nil {
			skipWritingCertToTranscript = true
			msg = processedMsg // msg is now a processed-by-extension certificateMsg
		}
	}
	// [UTLS SECTION ENDS]

	certMsg, ok := msg.(*certificateMsgTLS13)
	if !ok {
		c.sendAlert(alertUnexpectedMessage)
		return unexpectedMessageError(certMsg, msg)
	}
	if len(certMsg.certificate.Certificate) == 0 {
		c.sendAlert(alertDecodeError)
		return errors.New("tls: received empty certificates message")
	}
	// [UTLS SECTION BEGINS]
	if !skipWritingCertToTranscript { // write to transcript only if it is not compressedCert (i.e. if not processed by extension)
		if err = transcriptMsg(certMsg, hs.transcript); err != nil {
			return err
		}
	}
	// [UTLS SECTION ENDS]

	c.scts = certMsg.certificate.SignedCertificateTimestamps
	c.ocspResponse = certMsg.certificate.OCSPStaple

	if err := c.verifyServerCertificate(certMsg.certificate.Certificate); err != nil {
		return err
	}

	// certificateVerifyMsg is included in the transcript, but not until
	// after we verify the handshake signature, since the state before
	// this message was sent is used.
	msg, err = c.readHandshake(nil)
	if err != nil {
		return err
	}

	certVerify, ok := msg.(*certificateVerifyMsg)
	if !ok {
		c.sendAlert(alertUnexpectedMessage)
		return unexpectedMessageError(certVerify, msg)
	}

	// See RFC 8446, Section 4.4.3.
	if !isSupportedSignatureAlgorithm(certVerify.signatureAlgorithm, c.config.supportedSignatureAlgorithms()) { // [UTLS] ported from cloudflare/go
		c.sendAlert(alertIllegalParameter)
		return errors.New("tls: certificate used with invalid signature algorithm")
	}
	sigType, sigHash, err := typeAndHashFromSignatureScheme(certVerify.signatureAlgorithm)
	if err != nil {
		return c.sendAlert(alertInternalError)
	}
	if sigType == signaturePKCS1v15 || sigHash == crypto.SHA1 {
		c.sendAlert(alertIllegalParameter)
		return errors.New("tls: certificate used with invalid signature algorithm")
	}
	signed := signedMessage(sigHash, serverSignatureContext, hs.transcript)
	if err := verifyHandshakeSignature(sigType, c.peerCertificates[0].PublicKey,
		sigHash, signed, certVerify.signature); err != nil {
		c.sendAlert(alertDecryptError)
		return errors.New("tls: invalid signature by the server certificate: " + err.Error())
	}

	if err := transcriptMsg(certVerify, hs.transcript); err != nil {
		return err
	}

	return nil
}

func (hs *clientHandshakeStateTLS13) readServerFinished() error {
	c := hs.c

	// finishedMsg is included in the transcript, but not until after we
	// check the client version, since the state before this message was
	// sent is used during verification.
	msg, err := c.readHandshake(nil)
	if err != nil {
		return err
	}

	finished, ok := msg.(*finishedMsg)
	if !ok {
		c.sendAlert(alertUnexpectedMessage)
		return unexpectedMessageError(finished, msg)
	}

	expectedMAC := hs.suite.finishedHash(c.in.trafficSecret, hs.transcript)
	if !hmac.Equal(expectedMAC, finished.verifyData) {
		c.sendAlert(alertDecryptError)
		return errors.New("tls: invalid server finished hash")
	}

	if err := transcriptMsg(finished, hs.transcript); err != nil {
		return err
	}

	// Derive secrets that take context through the server Finished.

	hs.trafficSecret = hs.suite.deriveSecret(hs.masterSecret,
		clientApplicationTrafficLabel, hs.transcript)
	serverSecret := hs.suite.deriveSecret(hs.masterSecret,
		serverApplicationTrafficLabel, hs.transcript)
	c.in.setTrafficSecret(hs.suite, QUICEncryptionLevelApplication, serverSecret)

	err = c.config.writeKeyLog(keyLogLabelClientTraffic, hs.hello.random, hs.trafficSecret)
	if err != nil {
		c.sendAlert(alertInternalError)
		return err
	}
	err = c.config.writeKeyLog(keyLogLabelServerTraffic, hs.hello.random, serverSecret)
	if err != nil {
		c.sendAlert(alertInternalError)
		return err
	}

	c.ekm = hs.suite.exportKeyingMaterial(hs.masterSecret, hs.transcript)

	return nil
}

func (hs *clientHandshakeStateTLS13) sendClientCertificate() error {
	c := hs.c

	if hs.certReq == nil {
		return nil
	}

	cert, err := c.getClientCertificate(&CertificateRequestInfo{
		AcceptableCAs:    hs.certReq.certificateAuthorities,
		SignatureSchemes: hs.certReq.supportedSignatureAlgorithms,
		Version:          c.vers,
		ctx:              hs.ctx,
	})
	if err != nil {
		return err
	}

	certMsg := new(certificateMsgTLS13)

	certMsg.certificate = *cert
	certMsg.scts = hs.certReq.scts && len(cert.SignedCertificateTimestamps) > 0
	certMsg.ocspStapling = hs.certReq.ocspStapling && len(cert.OCSPStaple) > 0

	if _, err := hs.c.writeHandshakeRecord(certMsg, hs.transcript); err != nil {
		return err
	}

	// If we sent an empty certificate message, skip the CertificateVerify.
	if len(cert.Certificate) == 0 {
		return nil
	}

	certVerifyMsg := new(certificateVerifyMsg)
	certVerifyMsg.hasSignatureAlgorithm = true

	certVerifyMsg.signatureAlgorithm, err = selectSignatureScheme(c.vers, cert, hs.certReq.supportedSignatureAlgorithms)
	if err != nil {
		// getClientCertificate returned a certificate incompatible with the
		// CertificateRequestInfo supported signature algorithms.
		c.sendAlert(alertHandshakeFailure)
		return err
	}

	sigType, sigHash, err := typeAndHashFromSignatureScheme(certVerifyMsg.signatureAlgorithm)
	if err != nil {
		return c.sendAlert(alertInternalError)
	}

	signed := signedMessage(sigHash, clientSignatureContext, hs.transcript)
	signOpts := crypto.SignerOpts(sigHash)
	if sigType == signatureRSAPSS {
		signOpts = &rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthEqualsHash, Hash: sigHash}
	}
	sig, err := cert.PrivateKey.(crypto.Signer).Sign(c.config.rand(), signed, signOpts)
	if err != nil {
		c.sendAlert(alertInternalError)
		return errors.New("tls: failed to sign handshake: " + err.Error())
	}
	certVerifyMsg.signature = sig

	if _, err := hs.c.writeHandshakeRecord(certVerifyMsg, hs.transcript); err != nil {
		return err
	}

	return nil
}

func (hs *clientHandshakeStateTLS13) sendClientFinished() error {
	c := hs.c

	finished := &finishedMsg{
		verifyData: hs.suite.finishedHash(c.out.trafficSecret, hs.transcript),
	}

	if _, err := hs.c.writeHandshakeRecord(finished, hs.transcript); err != nil {
		return err
	}

	c.out.setTrafficSecret(hs.suite, QUICEncryptionLevelApplication, hs.trafficSecret)

	if !c.config.SessionTicketsDisabled && c.config.ClientSessionCache != nil {
		c.resumptionSecret = hs.suite.deriveSecret(hs.masterSecret,
			resumptionLabel, hs.transcript)
	}

	if c.quic != nil {
		if c.hand.Len() != 0 {
			c.sendAlert(alertUnexpectedMessage)
		}
		c.quicSetWriteSecret(QUICEncryptionLevelApplication, hs.suite.id, hs.trafficSecret)
	}

	return nil
}

func (c *Conn) handleNewSessionTicket(msg *newSessionTicketMsgTLS13) error {
	if !c.isClient {
		c.sendAlert(alertUnexpectedMessage)
		return errors.New("tls: received new session ticket from a client")
	}

	if c.config.SessionTicketsDisabled || c.config.ClientSessionCache == nil {
		return nil
	}

	// See RFC 8446, Section 4.6.1.
	if msg.lifetime == 0 {
		return nil
	}
	lifetime := time.Duration(msg.lifetime) * time.Second
	if lifetime > maxSessionTicketLifetime {
		c.sendAlert(alertIllegalParameter)
		return errors.New("tls: received a session ticket with invalid lifetime")
	}

	// RFC 9001, Section 4.6.1
	if c.quic != nil && msg.maxEarlyData != 0 && msg.maxEarlyData != 0xffffffff {
		c.sendAlert(alertIllegalParameter)
		return errors.New("tls: invalid early data for QUIC connection")
	}

	cipherSuite := cipherSuiteTLS13ByID(c.cipherSuite)
	if cipherSuite == nil || c.resumptionSecret == nil {
		return c.sendAlert(alertInternalError)
	}

	psk := cipherSuite.expandLabel(c.resumptionSecret, "resumption",
		msg.nonce, cipherSuite.hash.Size())

	session, err := c.sessionState()
	if err != nil {
		c.sendAlert(alertInternalError)
		return err
	}
	session.secret = psk
	session.useBy = uint64(c.config.time().Add(lifetime).Unix())
	session.ageAdd = msg.ageAdd
	session.EarlyData = c.quic != nil && msg.maxEarlyData == 0xffffffff // RFC 9001, Section 4.6.1
	cs := &ClientSessionState{ticket: msg.label, session: session}

	if cacheKey := c.clientSessionCacheKey(); cacheKey != "" {
		c.config.ClientSessionCache.Put(cacheKey, cs)
	}

	return nil
}
