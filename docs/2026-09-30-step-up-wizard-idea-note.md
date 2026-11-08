# Step-up setup wizard

Idea note, moved from BACKLOG.md. Not scheduled.

**Added:** 2026-09-30 · **Status:** idea

`token add/update -step-up` prints an `otpauth://` URL and a `qrencode` command
that is installed on neither aarni nor the Mac. Draw the QR in the terminal and
save only after one code from the authenticator verifies, so a mistyped secret
cannot lock an agent out.
