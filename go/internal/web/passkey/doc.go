// Package passkey is the WebAuthn cryptography of the web app's passkeys (§8.6, D61) on go-webauthn's protocol
// package: a registration's attestation and a login's assertion, checked as py_webauthn checked them, so the
// credentials it kept (base64url ids, COSE public keys, sign counts) keep working.
//
// The challenges, the rows and the rules of the ceremonies are internal/web's Auth; Verifier is what it calls.
//
// Until go.mod requires github.com/go-webauthn/webauthn, the verifier is built only with the tag `webauthn`; without
// it Verifier refuses every passkey (logins by the bot's code still work).
package passkey
