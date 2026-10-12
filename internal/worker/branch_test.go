package worker

import (
	"strings"
	"testing"

	"github.com/mhersson/contextmatrix-agent/internal/cmclient"
	"github.com/stretchr/testify/assert"
)

func TestCardBranchName(t *testing.T) {
	t.Parallel()

	jira := func(key string) cmclient.CardSource {
		return cmclient.CardSource{System: "jira", ExternalID: key}
	}

	atCap := "PROJ-1" + strings.Repeat("0", 58)   // 64 bytes
	overCap := "PROJ-1" + strings.Repeat("0", 59) // 65 bytes

	tests := []struct {
		name   string
		cardID string
		source cmclient.CardSource
		want   string
	}{
		{"no source", "CMX-001", cmclient.CardSource{}, "cm/cmx-001"},
		{"jira key appended uppercase", "CTXMAX-801", jira("PROJ-123"), "cm/ctxmax-801-PROJ-123"},
		{"jira key with digits and underscore", "CMX-001", jira("MY_PROJ2-7"), "cm/cmx-001-MY_PROJ2-7"},
		{"lowercase card id stays lowercase", "cmx-001", jira("ABC-1"), "cm/cmx-001-ABC-1"},
		{"key at the length cap", "CMX-001", jira(atCap), "cm/cmx-001-" + atCap},
		{"key over the length cap", "CMX-001", jira(overCap), "cm/cmx-001"},
		{"lowercase key rejected", "CMX-001", jira("proj-123"), "cm/cmx-001"},
		{"mixed-case key rejected", "CMX-001", jira("Proj-123"), "cm/cmx-001"},
		{"single-letter project rejected", "CMX-001", jira("P-1"), "cm/cmx-001"},
		{"leading-zero issue number rejected", "CMX-001", jira("PROJ-0123"), "cm/cmx-001"},
		{"zero issue number rejected", "CMX-001", jira("PROJ-0"), "cm/cmx-001"},
		{"missing issue number rejected", "CMX-001", jira("PROJ-"), "cm/cmx-001"},
		{"leading whitespace rejected", "CMX-001", jira(" PROJ-123"), "cm/cmx-001"},
		{"trailing newline rejected", "CMX-001", jira("PROJ-123\n"), "cm/cmx-001"},
		{"dot-dot rejected", "CMX-001", jira("PROJ-1..2"), "cm/cmx-001"},
		{"path separator rejected", "CMX-001", jira("PROJ-1/x"), "cm/cmx-001"},
		{"empty key rejected", "CMX-001", jira(""), "cm/cmx-001"},
		{"other source system ignored", "CMX-001", cmclient.CardSource{System: "github", ExternalID: "ABC-12"}, "cm/cmx-001"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tt.want, cardBranchName(tt.cardID, tt.source))
		})
	}
}
