/**
 * Looks for credentials in a plugin's data.json and REFUSES the plugin when it
 * finds one. It never edits.
 *
 * A filter that strips suspected secrets was proposed and rejected. Three
 * reasons, all of which apply here:
 *
 *  - Detectors cannot be right on arbitrary plugin JSON. A credential in
 *    "endpoint": "https://user:pass@host", a JWT under "config", or a key the
 *    author called "k" all evade name matching -- so this file checks values as
 *    well as names, and still makes no claim to completeness.
 *  - A filtered data.json is a broken file that looks fine. The plugin reads
 *    valid JSON with a hole and re-prompts, silently resets, or throws.
 *  - The real harm is false confidence. A feature promising that secrets are
 *    filtered gets enabled everywhere and then stops being thought about.
 *
 * The asymmetry is unforgiving: a false positive costs a setting, a false
 * negative puts a live key in git history permanently, and nothing reports it.
 * So on suspicion this refuses the plugin and says what it saw, and the user
 * may override per plugin having read that.
 *
 * Deliberately its own module with one entry point, so it can be removed in a
 * single commit if it proves not to earn its place.
 */

export type Suspicion = {
	/** Where it was found, e.g. "services[0].password". */
	path: string;
	/** What was suspicious, in words a person can act on. */
	why: string;
};

// Names only. Deliberately NOT including single letters: a key the author
// called "k" is exactly the case name matching cannot catch, and pretending
// otherwise is how a detector earns trust it has not got. The value detectors
// below are what catch that one.
const SUSPICIOUS_KEY =
	/(^|[^a-z])(token|secret|password|passwd|pwd|api[-_]?key|access[-_]?key|private[-_]?key|credential|auth|bearer|session)([^a-z]|$)|^key$/i;

/** user:pass@host in any URL. */
const URL_WITH_USERINFO = /^[a-z][a-z0-9+.-]*:\/\/[^/\s:@]+:[^/\s@]+@/i;

/** Three base64url segments separated by dots, starting with a JSON header. */
const JWT = /^eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+$/;

/** A long unbroken run of key-shaped characters and nothing else. */
const OPAQUE = /^[A-Za-z0-9_\-+/=.]{32,}$/;

/**
 * scanForSecrets walks a parsed data.json and returns everything suspicious.
 *
 * An empty array means nothing was recognised, NOT that the file is safe. That
 * distinction is why the result is called a suspicion and the UI says "nothing
 * recognised" rather than "no secrets".
 */
export function scanForSecrets(value: unknown): Suspicion[] {
	const out: Suspicion[] = [];
	walk(value, "", out);
	return out;
}

function walk(value: unknown, path: string, out: Suspicion[]): void {
	if (Array.isArray(value)) {
		value.forEach((v, i) => walk(v, `${path}[${i}]`, out));
		return;
	}
	if (value && typeof value === "object") {
		for (const [k, v] of Object.entries(value as Record<string, unknown>)) {
			const child = path ? `${path}.${k}` : k;
			if (typeof v === "string" && v.length > 0 && SUSPICIOUS_KEY.test(k)) {
				out.push({ path: child, why: `the key name "${k}" is what a credential is usually called` });
				continue;
			}
			walk(v, child, out);
		}
		return;
	}
	if (typeof value !== "string" || value.length === 0) return;

	if (URL_WITH_USERINFO.test(value)) {
		out.push({ path: path || "(root)", why: "a URL carrying a username and password" });
		return;
	}
	if (JWT.test(value)) {
		out.push({ path: path || "(root)", why: "a JWT, which usually is a live session" });
		return;
	}
	if (OPAQUE.test(value)) {
		out.push({
			path: path || "(root)",
			why: `a ${value.length}-character opaque string, which is the shape of a key rather than a setting`,
		});
	}
}
