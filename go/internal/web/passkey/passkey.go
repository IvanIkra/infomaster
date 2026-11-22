package passkey

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/protocol/webauthncose"
)

// algorithms are py_webauthn's supported_pub_key_algs: what a new passkey's key may be.
var algorithms = []webauthncose.COSEAlgorithmIdentifier{-8, -7, -36, -37, -38, -39, -257, -258, -259, -65535}

func params() []protocol.CredentialParameter {
	out := make([]protocol.CredentialParameter, 0, len(algorithms))
	for _, alg := range algorithms {
		out = append(out, protocol.CredentialParameter{Type: protocol.PublicKeyCredentialType, Algorithm: alg})
	}
	return out
}

// Verifier checks the ceremonies with go-webauthn.
type Verifier struct{}

// sameOrigin is py_webauthn's check of the client data's origin: the very text expected.
func sameOrigin(clientData []byte, origin string) error {
	var client struct {
		Origin string `json:"origin"`
	}
	if err := json.Unmarshal(clientData, &client); err != nil {
		return err
	}
	if client.Origin != origin {
		return fmt.Errorf("unexpected client data origin %q, expected %q", client.Origin, origin)
	}
	return nil
}

// Register checks a new passkey (navigator.credentials.create() as JSON) against the challenge given for it: its
// credential id, COSE public key and sign count as py_webauthn's verify_registration_response gives them.
func (Verifier) Register(credential []byte, challenge, rpID, origin string) ([]byte, []byte, int64, error) {
	parsed, err := protocol.ParseCredentialCreationResponseBytes(credential)
	if err != nil {
		return nil, nil, 0, err
	}
	if err := sameOrigin(parsed.Raw.AttestationResponse.ClientDataJSON, origin); err != nil {
		return nil, nil, 0, err
	}
	if _, err := parsed.Verify(challenge, rpID, []string{origin}, nil, nil,
		protocol.TopOriginImplicitVerificationMode, false, true, true, nil, params(), protocol.AttestationPolicy{},
		protocol.SignaturePolicy{}); err != nil {
		return nil, nil, 0, err
	}
	auth := parsed.Response.AttestationObject.AuthData
	if len(auth.AttData.CredentialID) == 0 || len(auth.AttData.CredentialPublicKey) == 0 {
		return nil, nil, 0, errors.New("no attested credential data")
	}
	return auth.AttData.CredentialID, auth.AttData.CredentialPublicKey, int64(auth.Counter), nil
}

// Login checks an assertion (navigator.credentials.get() as JSON) of a stored passkey: the new sign count. As
// py_webauthn, a count that did not grow is refused unless both counts are zero.
func (Verifier) Login(credential []byte, challenge, rpID, origin string, publicKey []byte, signCount int64) (int64,
	error) {
	parsed, err := protocol.ParseCredentialRequestResponseBytes(credential)
	if err != nil {
		return 0, err
	}
	if err := sameOrigin(parsed.Raw.AssertionResponse.ClientDataJSON, origin); err != nil {
		return 0, err
	}
	if err := parsed.Verify(challenge, rpID, "", []string{origin}, nil, nil,
		protocol.TopOriginImplicitVerificationMode, false, true, true, publicKey, protocol.SignaturePolicy{}); err != nil {
		return 0, err
	}
	count := int64(parsed.Response.AuthenticatorData.Counter)
	if (count > 0 || signCount > 0) && count <= signCount {
		return 0, fmt.Errorf("response sign count of %d was not greater than current count of %d", count, signCount)
	}
	return count, nil
}
