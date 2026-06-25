package taskskills

import (
	"encoding/base64"
	"os"
)

// gitAuthEnv returns the env for git subprocesses: the inherited env plus the
// token as an http.extraheader (base64 x-access-token, never written to disk).
func gitAuthEnv(token string) []string {
	env := append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if token == "" {
		return env
	}

	auth := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))

	return append(env,
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=http.extraheader",
		"GIT_CONFIG_VALUE_0=Authorization: Basic "+auth,
	)
}
