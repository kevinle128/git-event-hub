// Package route decides which sinks receive an event.
//
// An event belongs to the subscriber whose identity opened the pull request
// or merge request (Event.Author). The author of a check, pipeline or comment
// event comes from the hub's memory of the pull request, so every later event
// follows the owner. Subscribers may also follow GitHub repositories by glob;
// GitLab merge requests go only to their author. No other subscriber ever
// receives an event.
package route

import (
	"strings"

	"git-event-hub/internal/config"
	"git-event-hub/internal/glob"
	"git-event-hub/pkg/prevents"
)

// Reasons for a drop. They are metric labels, so the set stays small.
const (
	ReasonKind          = "kind_out_of_scope"
	ReasonCommentAuthor = "comment_author_out_of_scope"
	ReasonNoOwner       = "no_owner"
	ReasonNoSubscriber  = "no_subscriber"
	ReasonFiltered      = "filtered"
)

// Target is one sink that must receive an event.
type Target struct {
	Subscriber string
	Sink       string
}

// Key is the queue name of the target.
func (t Target) Key() string { return config.Key(t.Subscriber, t.Sink) }

// Router is built from one configuration and is safe for concurrent use.
type Router struct {
	kinds          map[prevents.Kind]bool
	commentAuthors glob.Set
	owners         map[prevents.Provider]map[string]int // login (lower case) -> index in subs
	subs           []subscriber
}

type subscriber struct {
	id            string
	filter        matcher
	subscriptions []subscription
	sinks         []sinkRoute
}

type subscription struct {
	repos glob.Set
	kinds map[prevents.Kind]bool
}

type sinkRoute struct {
	name   string
	filter matcher
}

// New compiles the routing of a configuration. Disabled subscribers and
// sinks are left out.
func New(c *config.Config) (*Router, error) {
	ca, err := glob.CompileSet(c.Defaults.CommentAuthors)
	if err != nil {
		return nil, err
	}
	r := &Router{
		kinds:          kindSet(c.Defaults.Kinds),
		commentAuthors: ca,
		owners:         map[prevents.Provider]map[string]int{},
	}
	for _, p := range prevents.Providers {
		r.owners[p] = map[string]int{}
	}
	for _, sc := range c.Subscribers {
		if !sc.On() {
			continue
		}
		f, err := newMatcher(sc.Filter)
		if err != nil {
			return nil, err
		}
		s := subscriber{id: sc.ID, filter: f}
		for _, sub := range sc.Subscriptions {
			repos, err := glob.CompileSet(sub.Repos)
			if err != nil {
				return nil, err
			}
			s.subscriptions = append(s.subscriptions, subscription{repos: repos, kinds: kindSet(sub.Kinds)})
		}
		for _, k := range sc.Sinks {
			if !k.On() {
				continue
			}
			kf, err := newMatcher(k.Filter)
			if err != nil {
				return nil, err
			}
			s.sinks = append(s.sinks, sinkRoute{name: k.Name, filter: kf})
		}
		idx := len(r.subs)
		r.subs = append(r.subs, s)
		for _, p := range prevents.Providers {
			for _, login := range sc.Identities.For(p) {
				r.owners[p][strings.ToLower(login)] = idx
			}
		}
	}
	return r, nil
}

// Route returns the sinks that must receive e, or none and the reason.
func (r *Router) Route(e prevents.Event) ([]Target, string) {
	if r.kinds != nil && !r.kinds[e.Kind] {
		return nil, ReasonKind
	}
	// A comment counts only when a bot account wrote it. Otherwise any
	// person could post a fake review block on a public repository.
	// A comment without a known author does not count either.
	if e.Kind == prevents.KindComment || e.Kind == prevents.KindReviewFinished {
		if c := e.Commenter(); c == "" || !r.commentAuthors.Any(c) {
			return nil, ReasonCommentAuthor
		}
	}
	picked := map[int]bool{}
	if e.Author != nil && *e.Author != "" {
		if idx, ok := r.owners[e.Provider][strings.ToLower(*e.Author)]; ok {
			picked[idx] = true
		}
	}
	if e.Provider == prevents.GitHub {
		for i, s := range r.subs {
			for _, sub := range s.subscriptions {
				if sub.repos.Any(e.Repo) && (sub.kinds == nil || sub.kinds[e.Kind]) {
					picked[i] = true
				}
			}
		}
	}
	if len(picked) == 0 {
		if e.Author == nil || *e.Author == "" {
			return nil, ReasonNoOwner
		}
		return nil, ReasonNoSubscriber
	}
	var out []Target
	// Walk in configuration order so the result is stable.
	for i, s := range r.subs {
		if !picked[i] || !s.filter.match(e) {
			continue
		}
		for _, k := range s.sinks {
			if k.filter.match(e) {
				out = append(out, Target{Subscriber: s.id, Sink: k.name})
			}
		}
	}
	if len(out) == 0 {
		return nil, ReasonFiltered
	}
	return out, ""
}

// Keys returns the queue names of every enabled sink.
func (r *Router) Keys() []string {
	var out []string
	for _, s := range r.subs {
		for _, k := range s.sinks {
			out = append(out, config.Key(s.id, k.name))
		}
	}
	return out
}

type matcher struct {
	providers        map[prevents.Provider]bool
	include, exclude glob.Set
	kinds            map[prevents.Kind]bool
}

func newMatcher(f config.Filter) (matcher, error) {
	inc, err := glob.CompileSet(f.Repos.Include)
	if err != nil {
		return matcher{}, err
	}
	exc, err := glob.CompileSet(f.Repos.Exclude)
	if err != nil {
		return matcher{}, err
	}
	m := matcher{include: inc, exclude: exc, kinds: kindSet(f.Kinds)}
	if len(f.Providers) > 0 {
		m.providers = map[prevents.Provider]bool{}
		for _, p := range f.Providers {
			m.providers[prevents.Provider(p)] = true
		}
	}
	return m, nil
}

func (m matcher) match(e prevents.Event) bool {
	if m.providers != nil && !m.providers[e.Provider] {
		return false
	}
	if m.kinds != nil && !m.kinds[e.Kind] {
		return false
	}
	if len(m.include) > 0 && !m.include.Any(e.Repo) {
		return false
	}
	return !m.exclude.Any(e.Repo)
}

func kindSet(kinds []string) map[prevents.Kind]bool {
	if len(kinds) == 0 {
		return nil
	}
	m := make(map[prevents.Kind]bool, len(kinds))
	for _, k := range kinds {
		m[prevents.Kind(k)] = true
	}
	return m
}
