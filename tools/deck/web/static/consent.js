// Answers a credential approval request (ADR-0018). The text shown comes from
// the same bytes the passkey signs. credential-approval decides whether an
// answer counts, so this page reports only that it was sent.
(function () {
  "use strict";

  const encodeB64Url = (buffer) =>
    btoa(String.fromCharCode(...new Uint8Array(buffer)))
      .replace(/\+/g, "-")
      .replace(/\//g, "_")
      .replace(/=+$/, "");

  const decodeB64Url = (text) => {
    const plain = atob(text.replace(/-/g, "+").replace(/_/g, "/"));
    return Uint8Array.from(plain, (c) => c.charCodeAt(0));
  };

  const state = (text) => {
    document.getElementById("consent-state").textContent = text;
  };

  const passkeyError = (error) =>
    error && error.name === "NotAllowedError"
      ? "The passkey request was cancelled or timed out. You can try again."
      : "The passkey request failed: " + (error && error.message ? error.message : "unknown error");

  const postJSON = (url, body) =>
    fetch(url, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
    });

  function answerPage(root) {
    const base = "/consent/" + root.dataset.consentId + "/";
    const actions = document.getElementById("consent-actions");
    const approve = document.getElementById("consent-approve");
    const deny = document.getElementById("consent-deny");
    let ended = false;
    let sent = "";

    const end = (text) => {
      ended = true;
      actions.hidden = true;
      state(text);
    };

    const follow = async () => {
      if (ended) {
        return;
      }
      try {
        const response = await fetch(base + "status", { cache: "no-store" });
        const { state: current } = await response.json();
        if (current === "gone") {
          // A request also ends by expiring or by the local prompt answering, so
          // this cannot say the answer was what ended it.
          end(sent ? "Your answer was sent and the request has ended." : "This request is no longer open.");
          return;
        }
        if (current === "rejected" && sent === "approve") {
          sent = "";
          approve.disabled = deny.disabled = false;
          state("That approval was not accepted: it was not signed by the pinned passkey.");
        }
      } catch (_) {
        // Keep following; a brief network failure ends nothing.
      }
      setTimeout(follow, 1500);
    };

    const load = async () => {
      const response = await fetch(base + "request", { cache: "no-store" });
      if (!response.ok) {
        end("This request is no longer open.");
        return;
      }
      const bytes = await response.arrayBuffer();
      const request = JSON.parse(new TextDecoder().decode(bytes));
      document.getElementById("consent-title").textContent = request.title;
      document.title = request.title;
      document.getElementById("consent-reason").textContent = request.reason;
      document.getElementById("consent-impact").textContent = request.impact;
      document.getElementById("consent-operation").textContent = request.operation;
      document.getElementById("consent-requester").textContent = request.requester;
      // credential-approval accepts only https, and this page checks it again,
      // since the spool's copy is what it reads.
      if (typeof request.requesterUrl === "string" && request.requesterUrl.startsWith("https://")) {
        const link = document.getElementById("consent-requester-link");
        link.href = request.requesterUrl;
        link.hidden = false;
      }
      document.getElementById("consent-expires").textContent = new Date(request.expiresAt * 1000).toLocaleTimeString();
      document.getElementById("consent-facts").hidden = false;
      actions.hidden = false;
      state("");

      approve.addEventListener("click", async () => {
        approve.disabled = deny.disabled = true;
        state("Waiting for your passkey.");
        try {
          const credential = await navigator.credentials.get({
            publicKey: {
              challenge: await crypto.subtle.digest("SHA-256", bytes),
              rpId: request.rpId,
              allowCredentials: [{ type: "public-key", id: decodeB64Url(request.credentialId) }],
              userVerification: "required",
              timeout: 120000,
            },
          });
          const sentResponse = await postJSON(base + "approve", {
            clientDataJSON: encodeB64Url(credential.response.clientDataJSON),
            authenticatorData: encodeB64Url(credential.response.authenticatorData),
            signature: encodeB64Url(credential.response.signature),
          });
          if (!sentResponse.ok) {
            throw new Error("the page could not pass on the approval");
          }
          sent = "approve";
          state("Checking the answer.");
        } catch (error) {
          approve.disabled = deny.disabled = false;
          state(passkeyError(error));
        }
      });

      deny.addEventListener("click", async () => {
        approve.disabled = deny.disabled = true;
        try {
          const sentResponse = await fetch(base + "deny", { method: "POST" });
          if (!sentResponse.ok) {
            throw new Error("the page could not pass on the answer");
          }
          sent = "deny";
          state("Sending your answer.");
        } catch (error) {
          approve.disabled = deny.disabled = false;
          state(error.message);
        }
      });

      follow();
    };

    load().catch(() => state("The request could not be loaded. Reload to try again."));
  }

  function enrollPage() {
    document.getElementById("consent-enroll-create").addEventListener("click", async () => {
      state("Waiting for your passkey.");
      try {
        const credential = await navigator.credentials.create({
          publicKey: {
            challenge: crypto.getRandomValues(new Uint8Array(32)),
            rp: { id: location.hostname, name: "deck approvals" },
            user: {
              id: crypto.getRandomValues(new Uint8Array(16)),
              name: "credential-approval",
              displayName: "Credential approval",
            },
            // ES256 only: credential-approval verifies P-256 signatures.
            pubKeyCredParams: [{ type: "public-key", alg: -7 }],
            authenticatorSelection: { residentKey: "required", userVerification: "required" },
            attestation: "none",
          },
        });
        const key = credential.response.getPublicKey();
        if (!key) {
          throw new Error("this browser did not return the public key");
        }
        document.getElementById("consent-enrolled-rp").textContent = location.hostname;
        document.getElementById("consent-enrolled-id").textContent = credential.id;
        document.getElementById("consent-enrolled-key").textContent = btoa(String.fromCharCode(...new Uint8Array(key)));
        document.getElementById("consent-enrolled").hidden = false;
        state("Created. Pin these values in the host's configuration.");
      } catch (error) {
        state(passkeyError(error));
      }
    });
  }

  const root = document.getElementById("consent");
  if (root) {
    answerPage(root);
  } else if (document.getElementById("consent-enroll")) {
    enrollPage();
  }
})();
