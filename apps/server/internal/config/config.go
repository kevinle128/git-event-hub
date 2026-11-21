// Package config reads and checks the hub configuration (YAML), and reloads it
// when the file changes.
//
// The file is parsed with Viper and checked with validator. Secrets never
// appear in the file: every secret is named by the environment variable that
// holds it (secretEnv, urlEnv, tokenEnv).
package config

import (
	"errors"
	"fmt"
	"net"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-playground/validator/v10"
	"github.com/spf13/viper"

	"git-event-hub/internal/glob"
	"git-event-hub/pkg/prevents"
)

// Config is the whole configuration. Server, Database and Log are read at
// start; a change to them needs a restart. The other parts reload while the
// process runs.
type Config struct {
	Server      Server       `mapstructure:"server"`
	Database    Database     `mapstructure:"database"`
	Log         Log          `mapstructure:"log"`
	API         API          `mapstructure:"api"`
	Sources     []Source     `mapstructure:"sources" validate:"required,min=1,dive"`
	Defaults    Defaults     `mapstructure:"defaults"`
	Retention   Retention    `mapstructure:"retention"`
	Subscribers []Subscriber `mapstructure:"subscribers" validate:"required,min=1,dive"`
}

// Server is the HTTP listener.
type Server struct {
	Host string `mapstructure:"host"`
	Port int    `mapstructure:"port" validate:"min=1,max=65535"`
	// MaxBodyBytes limits the size of a webhook body. Default 25 MiB.
	MaxBodyBytes int64 `mapstructure:"maxBodyBytes" validate:"min=1"`
}

// Addr is host:port, with an IPv6 host in brackets.
func (s Server) Addr() string { return net.JoinHostPort(s.Host, strconv.Itoa(s.Port)) }

// Database names the environment variable that holds the Postgres URL.
type Database struct {
	URLEnv string `mapstructure:"urlEnv" validate:"required"`
	// MaxOpenConns limits the connection pool. Default 10.
	MaxOpenConns int `mapstructure:"maxOpenConns" validate:"min=1"`
}

// Log configures zap.
type Log struct {
	Level  string `mapstructure:"level" validate:"oneof=debug info warn error"`
	Format string `mapstructure:"format" validate:"oneof=json console"`
}

// API protects the read endpoints (/api/*, /metrics). They carry repository
// names and comment bodies, so they need a bearer token. When the variable
// that TokenEnv names is empty, the endpoints answer 401 to every request.
type API struct {
	TokenEnv string `mapstructure:"tokenEnv" validate:"required"`
}

// Source is one webhook source. Name is the URL segment: POST /hooks/<name>.
type Source struct {
	Name string `mapstructure:"name" validate:"required,hubname"`
	// Type picks the adapter: githubapp or gitlab. Default: githubapp for
	// the name "github", gitlab for the name "gitlab".
	Type string `mapstructure:"type"`
	// Enabled turns the source on. Default true. A disabled source still
	// answers 202, so the forge does not mark the webhook as failing.
	Enabled *bool `mapstructure:"enabled"`
	// SecretEnv names the variable that holds the webhook secret (GitHub) or
	// the secret token (GitLab; several tokens may be separated by commas).
	SecretEnv string `mapstructure:"secretEnv" validate:"required"`
	// Repos limits the source to repositories that match these globs. An event
	// of any other repository is dropped and not remembered, so a token that
	// belongs to one group cannot speak for another. Empty means every
	// repository.
	Repos []string `mapstructure:"repos" validate:"dive,glob"`
}

// On reports whether the source is enabled.
func (s Source) On() bool { return s.Enabled == nil || *s.Enabled }

// AdapterType returns Type, or the default for the source name.
func (s Source) AdapterType() string {
	if s.Type != "" {
		return s.Type
	}
	if s.Name == "github" {
		return "githubapp"
	}
	return s.Name
}

// Defaults apply to every subscriber and sink.
type Defaults struct {
	// Kinds lists the event kinds the hub accepts at all. Empty means all.
	Kinds []string `mapstructure:"kinds" validate:"dive,eventkind"`
	// CommentAuthors lists globs of the accounts whose comments count. Only
	// their comments become comment or review.finished events, so a person
	// cannot fake a review block on a public repository. The default is
	// "*[bot]": a person cannot register a login that contains "[". A GitLab
	// bot account has no such suffix and must be listed by its exact name. A
	// pattern made only of "*" is refused.
	CommentAuthors []string `mapstructure:"commentAuthors" validate:"dive,glob,notall"`
	// ReviewAuthors lists globs of the accounts that write review blocks. Only
	// their comments become review.finished events. There is no default: with
	// an empty list every review.finished event is dropped.
	ReviewAuthors []string `mapstructure:"reviewAuthors" validate:"dive,glob,notall"`
	Delivery      Delivery `mapstructure:"delivery"`
}

// Delivery is the retry policy of a sink. A zero field of a sink takes the
// value from Defaults.Delivery.
type Delivery struct {
	// BatchSize is the largest number of events in one call. Default 20.
	BatchSize int `mapstructure:"batchSize" validate:"min=0,max=500"`
	// Timeout limits one call. Default 10s.
	Timeout time.Duration `mapstructure:"timeout" validate:"min=0"`
	// Backoff starts at Initial and doubles up to Max. Defaults 2s and 5m.
	Backoff Backoff `mapstructure:"backoff"`
	// MaxAttempts is the number of failed calls after which an event is
	// parked as failed, so one bad event does not stop the sink. Default 20.
	MaxAttempts int `mapstructure:"maxAttempts" validate:"min=0"`
	// MaxAge is how long an event may wait before the hub gives up on it.
	// Default 72h.
	MaxAge time.Duration `mapstructure:"maxAge" validate:"min=0"`
}

// Backoff is the retry wait.
type Backoff struct {
	Initial time.Duration `mapstructure:"initial" validate:"min=0"`
	Max     time.Duration `mapstructure:"max" validate:"min=0"`
}

// Retention says how long the hub keeps data, in days. Defaults: webhook
// receipts 14, events and delivery log 30, pull request memory 60.
type Retention struct {
	ReceiptsDays int `mapstructure:"receiptsDays" validate:"min=0"`
	EventsDays   int `mapstructure:"eventsDays" validate:"min=0"`
	IndexDays    int `mapstructure:"indexDays" validate:"min=0"`
}

// Subscriber is one team member (or one system) that receives events.
type Subscriber struct {
	ID      string `mapstructure:"id" validate:"required,hubname"`
	Enabled *bool  `mapstructure:"enabled"`
	// Identities are the accounts of the subscriber. A pull request or merge
	// request opened by one of them belongs to the subscriber, and so does
	// every later event of it.
	Identities Identities `mapstructure:"identities"`
	// Filter narrows what the subscriber receives.
	Filter Filter `mapstructure:"filter"`
	// Subscriptions add GitHub pull requests of other people, by repository
	// glob (for example a lead who follows a repository). GitLab merge
	// requests go only to their author.
	Subscriptions []Subscription `mapstructure:"subscriptions" validate:"dive"`
	Sinks         []Sink         `mapstructure:"sinks" validate:"required,min=1,dive"`
}

// On reports whether the subscriber is enabled.
func (s Subscriber) On() bool { return s.Enabled == nil || *s.Enabled }

// Identities are forge accounts. Matching ignores case.
type Identities struct {
	GitHub []string `mapstructure:"github" validate:"dive,required"`
	GitLab []string `mapstructure:"gitlab" validate:"dive,required"`
}

// For returns the accounts on one provider.
func (i Identities) For(p prevents.Provider) []string {
	switch p {
	case prevents.GitHub:
		return i.GitHub
	case prevents.GitLab:
		return i.GitLab
	}
	return nil
}

// Filter selects events. An empty field matches everything.
type Filter struct {
	Providers []string   `mapstructure:"providers" validate:"dive,provider"`
	Repos     RepoFilter `mapstructure:"repos"`
	Kinds     []string   `mapstructure:"kinds" validate:"dive,eventkind"`
}

// RepoFilter selects repositories by glob ("*" stops at "/", "**" does not).
// An empty Include means all repositories.
type RepoFilter struct {
	Include []string `mapstructure:"include" validate:"dive,glob"`
	Exclude []string `mapstructure:"exclude" validate:"dive,glob"`
}

// Subscription follows the GitHub pull requests of repositories.
type Subscription struct {
	Repos []string `mapstructure:"repos" validate:"required,min=1,dive,glob"`
	Kinds []string `mapstructure:"kinds" validate:"dive,eventkind"`
}

// Sink is one delivery target of a subscriber. Keys that are not listed here
// go to Settings and are read by the adapter of the sink type.
type Sink struct {
	Name     string         `mapstructure:"name" validate:"required,hubname"`
	Type     string         `mapstructure:"type" validate:"required"`
	Enabled  *bool          `mapstructure:"enabled"`
	Filter   Filter         `mapstructure:"filter"`
	Delivery Delivery       `mapstructure:"delivery"`
	Settings map[string]any `mapstructure:",remain"`
}

// On reports whether the sink is enabled.
func (s Sink) On() bool { return s.Enabled == nil || *s.Enabled }

// Key is the queue name of a sink: "<subscriber>/<sink>".
func Key(subscriber, sink string) string { return subscriber + "/" + sink }

var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)

var validate = newValidator()

func newValidator() *validator.Validate {
	v := validator.New(validator.WithRequiredStructEnabled())
	must := func(tag string, fn validator.Func) {
		if err := v.RegisterValidation(tag, fn); err != nil {
			panic(err)
		}
	}
	must("hubname", func(fl validator.FieldLevel) bool { return nameRE.MatchString(fl.Field().String()) })
	must("eventkind", func(fl validator.FieldLevel) bool { return prevents.ValidKind(prevents.Kind(fl.Field().String())) })
	must("provider", func(fl validator.FieldLevel) bool {
		return prevents.ValidProvider(prevents.Provider(fl.Field().String()))
	})
	must("glob", func(fl validator.FieldLevel) bool { _, err := glob.Compile(fl.Field().String()); return err == nil })
	must("notall", func(fl validator.FieldLevel) bool { return strings.Trim(fl.Field().String(), "*") != "" })
	return v
}

// Parse reads and checks a YAML configuration. Unknown keys are errors.
func Parse(data []byte) (*Config, error) {
	v := viper.New()
	v.SetConfigType("yaml")
	if err := v.ReadConfig(strings.NewReader(string(data))); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	var c Config
	if err := v.UnmarshalExact(&c); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	c.applyDefaults()
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// Load reads the file at path.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	return Parse(data)
}

func (c *Config) applyDefaults() {
	setStr(&c.Server.Host, "0.0.0.0")
	setInt(&c.Server.Port, 8080)
	if c.Server.MaxBodyBytes == 0 {
		c.Server.MaxBodyBytes = 25 << 20
	}
	setStr(&c.Database.URLEnv, "DATABASE_URL")
	setInt(&c.Database.MaxOpenConns, 10)
	setStr(&c.Log.Level, "info")
	setStr(&c.Log.Format, "json")
	setStr(&c.API.TokenEnv, "HUB_API_TOKEN")
	if c.Defaults.CommentAuthors == nil {
		c.Defaults.CommentAuthors = []string{"*[bot]"}
	}
	d := &c.Defaults.Delivery
	setInt(&d.BatchSize, 20)
	setDur(&d.Timeout, 10*time.Second)
	setDur(&d.Backoff.Initial, 2*time.Second)
	setDur(&d.Backoff.Max, 5*time.Minute)
	setInt(&d.MaxAttempts, 20)
	setDur(&d.MaxAge, 72*time.Hour)
	setInt(&c.Retention.ReceiptsDays, 14)
	setInt(&c.Retention.EventsDays, 30)
	setInt(&c.Retention.IndexDays, 60)
	for i := range c.Subscribers {
		for j := range c.Subscribers[i].Sinks {
			s := &c.Subscribers[i].Sinks[j].Delivery
			setInt(&s.BatchSize, d.BatchSize)
			setDur(&s.Timeout, d.Timeout)
			setDur(&s.Backoff.Initial, d.Backoff.Initial)
			setDur(&s.Backoff.Max, d.Backoff.Max)
			setInt(&s.MaxAttempts, d.MaxAttempts)
			setDur(&s.MaxAge, d.MaxAge)
		}
	}
}

func setStr(p *string, v string) {
	if *p == "" {
		*p = v
	}
}

func setInt(p *int, v int) {
	if *p == 0 {
		*p = v
	}
}

func setDur(p *time.Duration, v time.Duration) {
	if *p == 0 {
		*p = v
	}
}

// Validate checks the values that do not need the adapters.
func (c *Config) Validate() error {
	if err := validate.Struct(c); err != nil {
		return fmt.Errorf("config: %w", friendly(err))
	}
	if err := c.Defaults.Delivery.check("defaults.delivery"); err != nil {
		return err
	}
	sources := map[string]bool{}
	for _, s := range c.Sources {
		if sources[s.Name] {
			return fmt.Errorf("config: source %q is listed twice", s.Name)
		}
		sources[s.Name] = true
	}
	subs := map[string]bool{}
	owner := map[string]string{} // "provider:login" -> subscriber id
	for _, s := range c.Subscribers {
		if subs[s.ID] {
			return fmt.Errorf("config: subscriber %q is listed twice", s.ID)
		}
		subs[s.ID] = true
		if s.On() {
			for _, p := range prevents.Providers {
				for _, login := range s.Identities.For(p) {
					k := string(p) + ":" + strings.ToLower(login)
					if other, dup := owner[k]; dup {
						return fmt.Errorf("config: %s identity %q belongs to both %q and %q", p, login, other, s.ID)
					}
					owner[k] = s.ID
				}
			}
		}
		sinks := map[string]bool{}
		for _, k := range s.Sinks {
			if sinks[k.Name] {
				return fmt.Errorf("config: subscriber %s has sink %q twice", s.ID, k.Name)
			}
			sinks[k.Name] = true
			if err := k.Delivery.check("subscriber " + s.ID + " sink " + k.Name + " delivery"); err != nil {
				return err
			}
		}
	}
	return nil
}

func (d Delivery) check(where string) error {
	if d.Backoff.Max < d.Backoff.Initial {
		return fmt.Errorf("config: %s: backoff.max is below backoff.initial", where)
	}
	return nil
}

// friendly turns validator errors into one line per field.
func friendly(err error) error {
	var verrs validator.ValidationErrors
	if !errors.As(err, &verrs) {
		return err
	}
	msgs := make([]string, 0, len(verrs))
	for _, fe := range verrs {
		field := strings.TrimPrefix(fe.Namespace(), "Config.")
		switch fe.Tag() {
		case "required":
			msgs = append(msgs, field+" is required")
		case "hubname":
			msgs = append(msgs, fmt.Sprintf("%s %q is not a lower-case name (a-z, 0-9, - and _)", field, fe.Value()))
		case "eventkind":
			msgs = append(msgs, fmt.Sprintf("%s %q is not an event kind (%v)", field, fe.Value(), prevents.Kinds))
		case "provider":
			msgs = append(msgs, fmt.Sprintf("%s %q is not a provider (github, gitlab)", field, fe.Value()))
		case "glob":
			msgs = append(msgs, fmt.Sprintf("%s %q is not a valid glob", field, fe.Value()))
		case "notall":
			msgs = append(msgs, fmt.Sprintf("%s %q matches every account; list the bot accounts", field, fe.Value()))
		default:
			msgs = append(msgs, fmt.Sprintf("%s fails %s=%s (value %v)", field, fe.Tag(), fe.Param(), fe.Value()))
		}
	}
	return errors.New(strings.Join(msgs, "; "))
}
