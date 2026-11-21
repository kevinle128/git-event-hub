package route

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"git-event-hub/internal/config"
	"git-event-hub/pkg/prevents"
)

const team = `
sources:
  - {name: github, secretEnv: GH}
  - {name: gitlab, secretEnv: GL}
defaults:
  kinds: [opened, ci.finished, review.finished, comment, merged]
subscribers:
  - id: kevin
    identities: {github: [kevinle128], gitlab: [kevin.le]}
    filter: {repos: {exclude: ["kevinle128/private-*"]}}
    sinks:
      - {name: omb, type: omb}
      - {name: audit, type: webhook, filter: {kinds: [merged]}}
      - {name: off, type: webhook, enabled: false}
  - id: lan
    identities: {github: [lan-vtc], gitlab: [lan.nguyen]}
    subscriptions:
      - {repos: ["kevinle128/git-event-hub"], kinds: [opened, merged]}
    sinks:
      - {name: hermes, type: webhook}
  - id: gone
    enabled: false
    identities: {github: [gone-user]}
    sinks: [{name: x, type: webhook}]
`

func router(t *testing.T) *Router {
	t.Helper()
	c, err := config.Parse([]byte(team))
	require.NoError(t, err)
	r, err := New(c)
	require.NoError(t, err)
	return r
}

func ev(p prevents.Provider, repo, author string, kind prevents.Kind, data any) prevents.Event {
	e := prevents.Event{Provider: p, Repo: repo, Number: 1, Kind: kind, Data: data}
	if author != "" {
		e.Author = &author
	}
	return e
}

func keys(ts []Target) []string {
	out := []string{}
	for _, t := range ts {
		out = append(out, t.Key())
	}
	return out
}

func TestRoute(t *testing.T) {
	r := router(t)
	opened := prevents.OpenedData{}
	bot := prevents.CommentData{Commenter: "ocr-review[bot]"}
	tests := []struct {
		name   string
		e      prevents.Event
		want   []string
		reason string
	}{
		{"owner gets own PR", ev(prevents.GitHub, "kevinle128/OpenMausBot", "kevinle128", prevents.KindOpened, opened), []string{"kevin/omb"}, ""},
		{"identity matching ignores case", ev(prevents.GitHub, "kevinle128/OpenMausBot", "KevinLe128", prevents.KindOpened, opened), []string{"kevin/omb"}, ""},
		{"sink filter adds the audit sink", ev(prevents.GitHub, "kevinle128/OpenMausBot", "kevinle128", prevents.KindMerged, prevents.MergedData{}), []string{"kevin/omb", "kevin/audit"}, ""},
		{"other member's PR is not delivered to kevin", ev(prevents.GitHub, "vtc/game", "lan-vtc", prevents.KindOpened, opened), []string{"lan/hermes"}, ""},
		{"subscription adds lan on a followed repo", ev(prevents.GitHub, "kevinle128/git-event-hub", "kevinle128", prevents.KindOpened, opened), []string{"kevin/omb", "lan/hermes"}, ""},
		{"subscription kinds limit what lan follows", ev(prevents.GitHub, "kevinle128/git-event-hub", "kevinle128", prevents.KindCIFinished, prevents.CIFinishedData{}), []string{"kevin/omb"}, ""},
		{"subscription follows a PR of a non-member", ev(prevents.GitHub, "kevinle128/git-event-hub", "stranger", prevents.KindOpened, opened), []string{"lan/hermes"}, ""},
		{"gitlab MR goes to its author only", ev(prevents.GitLab, "vtc/3q/cms", "kevin.le", prevents.KindOpened, opened), []string{"kevin/omb"}, ""},
		{"gitlab MR of a non-member is dropped", ev(prevents.GitLab, "vtc/3q/cms", "someone.else", prevents.KindOpened, opened), nil, ReasonNoSubscriber},
		{"gitlab subscriptions do not exist", ev(prevents.GitLab, "kevinle128/git-event-hub", "someone.else", prevents.KindOpened, opened), nil, ReasonNoSubscriber},
		{"unknown author is dropped", ev(prevents.GitHub, "vtc/game", "", prevents.KindCIFinished, prevents.CIFinishedData{}), nil, ReasonNoOwner},
		{"subscriber filter excludes a repo", ev(prevents.GitHub, "kevinle128/private-notes", "kevinle128", prevents.KindOpened, opened), nil, ReasonFiltered},
		{"kind out of scope", ev(prevents.GitHub, "kevinle128/OpenMausBot", "kevinle128", prevents.KindPushed, prevents.PushedData{}), nil, ReasonKind},
		{"bot comment passes", ev(prevents.GitHub, "kevinle128/OpenMausBot", "kevinle128", prevents.KindComment, bot), []string{"kevin/omb"}, ""},
		{"gitlab bot user passes", ev(prevents.GitLab, "vtc/3q/cms", "kevin.le", prevents.KindComment, prevents.CommentData{Commenter: "ocr-review-bot"}), []string{"kevin/omb"}, ""},
		{"human comment is dropped", ev(prevents.GitHub, "kevinle128/OpenMausBot", "kevinle128", prevents.KindComment, prevents.CommentData{Commenter: "mallory"}), nil, ReasonCommentAuthor},
		{"disabled subscriber gets nothing", ev(prevents.GitHub, "a/b", "gone-user", prevents.KindOpened, opened), nil, ReasonNoSubscriber},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, reason := r.Route(tt.e)
			assert.Equal(t, tt.reason, reason)
			if tt.want == nil {
				assert.Empty(t, got)
				return
			}
			assert.Equal(t, tt.want, keys(got))
		})
	}
}

func TestKeysListsEnabledSinks(t *testing.T) {
	assert.Equal(t, []string{"kevin/omb", "kevin/audit", "lan/hermes"}, router(t).Keys())
}
