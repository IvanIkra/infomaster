// Passkeys (WebAuthn) between the server's JSON options and the browser's ArrayBuffers.

export const supported = () => typeof window.PublicKeyCredential === "function" && !!navigator.credentials;

const toBuffer = (text) => {
  const base64 = text.replace(/-/g, "+").replace(/_/g, "/") + "===".slice((text.length + 3) % 4);
  return Uint8Array.from(atob(base64), (c) => c.charCodeAt(0)).buffer;
};

const toText = (buffer) => btoa(String.fromCharCode(...new Uint8Array(buffer)))
  .replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");

function credentialJSON(credential) {
  if (typeof credential.toJSON === "function") return credential.toJSON();
  const r = credential.response, response = { clientDataJSON: toText(r.clientDataJSON) };
  if (r.attestationObject) {
    response.attestationObject = toText(r.attestationObject);
    if (typeof r.getTransports === "function") response.transports = r.getTransports();
  } else {
    response.authenticatorData = toText(r.authenticatorData);
    response.signature = toText(r.signature);
    if (r.userHandle) response.userHandle = toText(r.userHandle);
  }
  return { id: credential.id, rawId: toText(credential.rawId), type: credential.type, response,
           clientExtensionResults: credential.getClientExtensionResults?.() ?? {},
           authenticatorAttachment: credential.authenticatorAttachment ?? undefined };
}

export async function create(options) {
  const publicKey = { ...options, challenge: toBuffer(options.challenge),
                      user: { ...options.user, id: toBuffer(options.user.id) },
                      excludeCredentials: (options.excludeCredentials || []).map((c) => ({ ...c, id: toBuffer(c.id) })) };
  return credentialJSON(await navigator.credentials.create({ publicKey }));
}

export async function get(options) {
  const publicKey = { ...options, challenge: toBuffer(options.challenge),
                      allowCredentials: (options.allowCredentials || []).map((c) => ({ ...c, id: toBuffer(c.id) })) };
  return credentialJSON(await navigator.credentials.get({ publicKey }));
}
