# Browser testing skill

Use for all browser-led Omahab checks. Applies to first-run setup,
dashboard journeys, application interfaces, and clarity measurements.

## Setup

- Start from the run `context.md`. Note the run identifier and agent name.
- Use a fresh browser context or process per test identity.
- Separate tabs alone do not isolate cookies. Use separate contexts.
- Set Chromium desktop viewport to 1440x900 first, then repeat key flows
  at 390x844.
- Record browser version in the run inventory.
- If browser tools are unavailable, mark browser-required cases `BLOCKED`
  with reason `browser tools unavailable`. Never mark them passed via API.

## Passkeys

- Use a fresh browser context with cache disabled before enrollment.
- Bring the Pocket ID page to the foreground before a trusted click.
- For browser automation enable CDP WebAuthn and create a CTAP2 internal
  authenticator with `hasResidentKey`, `hasUserVerification`,
  `isUserVerified`, and `automaticPresenceSimulation` set to true.
- Record secure-context status and `document.hasFocus()` without
  secret-bearing page captures.
- Require creation of a credential followed by logout and passkey login.
- A browser without the required authenticator remains `BLOCKED`, not
  `FAIL`.
- Never change `welcome.tsx` to repair a Pocket ID button. If the silent
  button also fails in a supported focused browser, retain it as an
  upstream Pocket ID finding with version, sanitized console output, and
  failed network or WebAuthn activity before proposing any upstream patch.
## Interaction

- Open the target URL with `browser.open` and keep the tab handle.
- Run `observe` before interaction and re-observe after each navigation.
- Prefer direct helpers (`click`, `type`, `fill`, `press`, `scroll`,
  `select`, `uploadFile`) over `tab.run`.
- Use `tab.run` only where direct helpers cannot perform the action.
- For `<select>` elements use `tab.select`. `tab.fill` does not support them.
- After navigation or re-render, observed ids and refs are invalid.
  Re-observe, then act in the same cell where possible.

## Accessibility

- Check keyboard-only operation for every main flow.
- Verify visible focus, form labels, dialog focus trap, and focus return
  after destructive dialogs.
- Check contrast and 200 percent zoom for key pages.
- Test Cancel, Escape, incorrect confirmation, and correct confirmation
  for destructive dialogs.

## Screenshots and evidence

- Take screenshots for visual confirmation of each journey.
- Save sanitized screenshots under the run `evidence/` directory.
- Pause screenshots and DOM capture during secret entry, invitation
  display, and recovery display.
- Resume capture only after sensitive content is removed or masked.
- Never record authorization headers, cookies, OAuth callbacks, token
  fragments, or secret-bearing response bodies.
- Label controlled request-failure evidence as simulated.
- Close owned browser sessions after their cases.

## Failure evidence

- Record reproduction steps, expected behavior, actual behavior, source
  and application versions, and sanitized evidence for each finding.
- Distinguish broken browser actions from API state. Do not replace a
  broken browser action with an API call and mark the browser case passed.
- Verify results afterward with API or service checks where the case
  allows `LIVE_API` in addition to `BROWSER`.

## Secret masking

- Use the coordinator-owned secret-input helper for credential fields.
- The helper accepts a field name and browser target, never a secret value
  in tool arguments.
- Restrict browser profiles and private console logs.
- Never use the user logged-in browser without specific authorization.
