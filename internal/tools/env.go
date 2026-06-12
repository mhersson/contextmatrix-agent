package tools

import "os"

// envAllowlist is the set of variables subprocesses spawned on behalf of the
// model may inherit. Everything else — API keys, MCP credentials, git tokens —
// is withheld; code-driven git (the worker's clone/push) injects credentials
// per invocation instead.
var envAllowlist = []string{"PATH", "HOME", "USER", "LANG", "LC_ALL", "TMPDIR", "TERM"}

// ScrubbedEnv builds an explicit cmd.Env from the allowlist plus extra
// "KEY=VALUE" entries (e.g. GOCACHE for toolchain subprocesses). Extras are
// appended last, so on a key collision they override allowlist values.
func ScrubbedEnv(extra []string) []string {
	env := make([]string, 0, len(envAllowlist)+len(extra))

	for _, k := range envAllowlist {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}

	return append(env, extra...)
}
