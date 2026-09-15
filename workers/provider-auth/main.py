#!/usr/bin/env python3
"""omahab-provider-auth: pinned LiteLLM subscription authenticators.

Runs with the same pinned LiteLLM Python environment as the native gateway
(nix/litellm-python.nix) and emits a small NDJSON protocol on stdout:

  {"event": "capabilities", ...}
  {"event": "challenge", "provider": ..., "verification_url": ...,
   "user_code"?: ..., "expires_at"?: ..., "callback_port"?: ...}
  {"event": "complete", "provider": ...}
  {"event": "error", "provider": ..., "code": ..., "message": ...}
  {"event": "status", "provider": ..., "authenticated": bool}

Never tokens, device-auth IDs, private verifier material, or callback
codes: only safe metadata crosses the pipe. The gateway owns one subprocess
per provider, cancellation/deadlines, and opaque session IDs.

Token locations come from CHATGPT_TOKEN_DIR / XAI_OAUTH_TOKEN_DIR, set
identically on helper and gateway. Directories are 0700, auth files are
written atomically mode 0600.
"""

import json
import os
import sys
import time


def emit(obj):
    sys.stdout.write(json.dumps(obj) + "\n")
    sys.stdout.flush()


def fail(provider, code, message):
    emit({"event": "error", "provider": provider, "code": code, "message": str(message)[:500]})
    return 1


def cmd_capabilities():
    out = {"event": "capabilities"}
    try:
        import litellm

        out["litellm_version"] = getattr(litellm, "__version__", "unknown")
    except Exception as exc:
        return fail("", "import", "litellm import failed: %s" % exc)
    try:
        from litellm.llms.chatgpt import authenticator as chatgpt_auth
        from litellm.llms.chatgpt import common_utils as chatgpt_common

        out["chatgpt"] = {
            "ok": True,
            "verify_url": chatgpt_common.CHATGPT_DEVICE_VERIFY_URL,
            "timeout_seconds": chatgpt_auth.DEVICE_CODE_TIMEOUT_SECONDS,
        }
    except Exception as exc:
        out["chatgpt"] = {"ok": False, "error": str(exc)[:200]}
    try:
        from litellm.llms.xai import oauth as xai_oauth

        out["xai"] = {
            "ok": True,
            "callback_port": xai_oauth.XAI_OAUTH_REDIRECT_PORT,
            "callback_timeout_seconds": xai_oauth.XAI_OAUTH_CALLBACK_TIMEOUT_SECONDS,
        }
    except Exception as exc:
        out["xai"] = {"ok": False, "error": str(exc)[:200]}
    emit(out)
    if not out["chatgpt"].get("ok") or not out["xai"].get("ok"):
        return 1
    return 0


def _status_of(exc):
    return getattr(exc, "status_code", None)


def cmd_chatgpt_login():
    provider = "chatgpt"
    try:
        from litellm.llms.chatgpt import authenticator as mod
        from litellm.llms.chatgpt import common_utils as common
    except Exception as exc:
        return fail(provider, "import", "chatgpt authenticator import failed: %s" % exc)

    import stat as statmod

    class OmahabChatGPT(mod.Authenticator):
        def _ensure_token_dir(self):
            os.makedirs(self.token_dir, mode=0o700, exist_ok=True)
            try:
                os.chmod(self.token_dir, 0o700)
            except OSError:
                pass

        def _write_auth_file(self, data):
            self._ensure_token_dir()
            tmp = os.path.join(self.token_dir, ".auth.json.%d.tmp" % os.getpid())
            fd = os.open(tmp, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
            try:
                with os.fdopen(fd, "w") as f:
                    json.dump(data, f)
                    f.flush()
                    os.fsync(f.fileno())
                os.replace(tmp, self.auth_file)
                os.chmod(self.auth_file, 0o600)
            except Exception:
                try:
                    os.unlink(tmp)
                except OSError:
                    pass
                raise

        def login_device(self, on_challenge):
            device_code = self._request_device_code()
            self._record_device_code_request()
            # Emit the real device challenge instead of printing it.
            on_challenge(
                verification_url=common.CHATGPT_DEVICE_VERIFY_URL,
                user_code=device_code["user_code"],
                expires_at=time.time() + mod.DEVICE_CODE_TIMEOUT_SECONDS,
            )
            auth_code = self._poll_for_authorization_code(device_code)
            tokens = self._exchange_code_for_tokens(auth_code)
            auth_data = self._build_auth_record(tokens)
            self._write_auth_file(auth_data)

    auth = OmahabChatGPT()
    challenged = {}

    def on_challenge(**kwargs):
        challenged.update(kwargs)
        emit(
            {
                "event": "challenge",
                "provider": provider,
                "verification_url": kwargs["verification_url"],
                "user_code": kwargs["user_code"],
                "expires_at": kwargs["expires_at"],
            }
        )

    try:
        auth.login_device(on_challenge)
    except Exception as exc:
        status = _status_of(exc)
        msg = str(exc)
        if status == 408 or "timed out" in msg.lower() or "expired" in msg.lower():
            return fail(provider, "expired", "device authorization expired: %s" % msg)
        if status == 403:
            return fail(provider, "not_entitled", "device authorization refused (403): %s" % msg)
        if status == 429:
            return fail(provider, "quota_exceeded", "device authorization rate limited (429)")
        return fail(provider, "error", "chatgpt login failed: %s" % msg)
    emit({"event": "complete", "provider": provider})
    _ = statmod
    return 0


def cmd_chatgpt_status():
    provider = "chatgpt"
    try:
        from litellm.llms.chatgpt import authenticator as mod
    except Exception as exc:
        return fail(provider, "import", "chatgpt authenticator import failed: %s" % exc)
    try:
        auth = mod.Authenticator()
        data = auth._read_auth_file()
        if not data or not data.get("access_token"):
            emit({"event": "status", "provider": provider, "authenticated": False})
            return 0
        try:
            auth.get_access_token()
        except Exception as exc:
            msg = str(exc).lower()
            if "re-login" in msg or "refresh" in msg or "login" in msg:
                emit({"event": "status", "provider": provider, "authenticated": False})
                return 0
            return fail(provider, "error", "chatgpt status failed: %s" % exc)
        emit({"event": "status", "provider": provider, "authenticated": True})
        return 0
    except Exception as exc:
        return fail(provider, "error", "chatgpt status failed: %s" % exc)


def cmd_xai_login():
    provider = "xai"
    try:
        from litellm.llms.xai import oauth as mod
    except Exception as exc:
        return fail(provider, "import", "xai authenticator import failed: %s" % exc)

    captured = {}

    class OmahabXAI(mod.XAIOAuthAuthenticator):
        def _build_authorize_url(self, **kwargs):
            url = super()._build_authorize_url(**kwargs)
            captured["url"] = url
            emit(
                {
                    "event": "challenge",
                    "provider": provider,
                    "verification_url": url,
                    "callback_port": mod.XAI_OAUTH_REDIRECT_PORT,
                }
            )
            return url

        def _start_callback_server(self, state):
            # Require 127.0.0.1:56121: upstream otherwise falls back to a
            # random port and breaks the relay. Fail clearly if occupied.
            from litellm.llms.xai.oauth import (
                _CallbackHandler,
                _CallbackServer,
                XAI_OAUTH_REDIRECT_HOST,
                XAI_OAUTH_REDIRECT_PATH,
                XAI_OAUTH_REDIRECT_PORT,
            )

            try:
                server = _CallbackServer((XAI_OAUTH_REDIRECT_HOST, XAI_OAUTH_REDIRECT_PORT), _CallbackHandler)
            except OSError as exc:
                raise mod.XAIOAuthError(
                    "xAI OAuth callback port %d is occupied (another login or tunnel holds it); free it and retry"
                    % XAI_OAUTH_REDIRECT_PORT
                ) from exc
            server.expected_state = state
            server.callback_result = None
            redirect_uri = "http://%s:%d%s" % (XAI_OAUTH_REDIRECT_HOST, XAI_OAUTH_REDIRECT_PORT, XAI_OAUTH_REDIRECT_PATH)
            return server, redirect_uri

    auth = OmahabXAI()
    try:
        auth.login(no_browser=True)
    except Exception as exc:
        msg = str(exc)
        lower = msg.lower()
        if "state mismatch" in lower:
            return fail(provider, "state_mismatch", "xAI OAuth state mismatch: %s" % msg)
        if "timed out" in lower or "timeout" in lower:
            return fail(provider, "expired", "xAI OAuth callback timed out: %s" % msg)
        if "occupied" in lower:
            return fail(provider, "callback_occupied", msg)
        if "authorization failed" in lower or "no code" in lower:
            return fail(provider, "not_entitled", "xAI authorization failed: %s" % msg)
        status = _status_of(exc)
        if status == 403:
            return fail(provider, "not_entitled", "xAI refused authorization (403): %s" % msg)
        if status == 429:
            return fail(provider, "quota_exceeded", "xAI rate limited (429)")
        return fail(provider, "error", "xai login failed: %s" % msg)
    emit({"event": "complete", "provider": provider})
    return 0


def cmd_xai_status():
    provider = "xai"
    try:
        from litellm.llms.xai import oauth as mod
    except Exception as exc:
        return fail(provider, "import", "xai authenticator import failed: %s" % exc)
    try:
        auth = mod.XAIOAuthAuthenticator()
        data = auth._read_auth_file()
        if not data or not data.get("access_token"):
            emit({"event": "status", "provider": provider, "authenticated": False})
            return 0
        try:
            auth.get_access_token()
        except mod.XAIOAuthLoginRequiredError:
            emit({"event": "status", "provider": provider, "authenticated": False})
            return 0
        except Exception as exc:
            return fail(provider, "error", "xai status failed: %s" % exc)
        emit({"event": "status", "provider": provider, "authenticated": True})
        return 0
    except Exception as exc:
        return fail(provider, "error", "xai status failed: %s" % exc)


def main(argv):
    if len(argv) < 2:
        sys.stderr.write("usage: omahab-provider-auth <capabilities|chatgpt-login|chatgpt-status|xai-login|xai-status>\n")
        return 2
    cmd = argv[1]
    if cmd == "capabilities":
        return cmd_capabilities()
    if cmd == "chatgpt-login":
        return cmd_chatgpt_login()
    if cmd == "chatgpt-status":
        return cmd_chatgpt_status()
    if cmd == "xai-login":
        return cmd_xai_login()
    if cmd == "xai-status":
        return cmd_xai_status()
    sys.stderr.write("unknown command: %s\n" % cmd)
    return 2


if __name__ == "__main__":
    sys.exit(main(sys.argv))
