package live

// A hosted live-view service makes the project itself. The first run against it
// asks for one (POST /v1/projects) and keeps what comes back: a project id and a
// push token for pushing, and a view key for the page address. The id and the
// push token are never in a page address, and the page address is the only thing
// that lets someone read the run.
//
// A service on this machine ("local" mode) keeps the older way: the identity
// made here is the project, and the service creates it at the first push. That
// way stays exactly as it was, so a bridge started with no flags needs no setup.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// sourceName labels the pushes of this tool at the service.
const sourceName = "vegaload"

// envPrefix starts the three variables that give a run a project without a
// file: VEGALOAD_LIVE_PROJECT, _TOKEN and _VIEW_KEY. CI jobs have no config
// directory that outlives them; without these every job would be a new project.
const envPrefix = "VEGALOAD_LIVE_"

// Issued is a project the service made for this machine.
type Issued struct {
	ProjectID        string    `json:"projectId"`
	PushToken        string    `json:"pushToken"`
	ViewKey          string    `json:"viewKey"`
	RetentionSeconds int       `json:"retentionSeconds,omitempty"` // how long the service keeps an idle project
	CreatedAt        time.Time `json:"createdAt"`
}

// Binding is the project one run sends to, whichever way it was found.
type Binding struct {
	ProjectID string
	PushToken string
	ViewKey   string // empty for a project the machine made itself: the id opens the page
	// Retention is how long the service keeps the project after its last push,
	// when it said so. Zero is unknown.
	Retention time.Duration
	// Hosted is a project the service made (or the environment named), whose page
	// address is a secret to be shared on purpose.
	Hosted bool
}

var (
	idPattern    = regexp.MustCompile(`^[A-Za-z0-9_-]{8,64}$`)
	keyPattern   = regexp.MustCompile(`^[A-Za-z0-9_-]{16,128}$`)
	tokenPattern = regexp.MustCompile(`^[A-Za-z0-9._~+/=-]{16,256}$`)
)

// valid checks what came from the network or the environment before it is put
// in a URL or a header.
func (is *Issued) valid() bool {
	return idPattern.MatchString(is.ProjectID) && keyPattern.MatchString(is.ViewKey) && tokenPattern.MatchString(is.PushToken)
}

// PushURL is where the events of this project are sent.
func (b Binding) PushURL(base *url.URL) string {
	return base.String() + "/v1/projects/" + b.ProjectID + "/ingest?source=" + sourceName
}

// PageURL is the page that shows the latest run of this project.
func (b Binding) PageURL(base *url.URL) string {
	if b.ViewKey != "" {
		return base.String() + "/p/" + b.ViewKey
	}
	return base.String() + "/p/" + b.ProjectID
}

// Notice is what a person is told about a hosted project, once, where the link
// is shown: who can see it and for how long. It is empty for a project on this
// machine.
func (b Binding) Notice() string {
	if !b.Hosted {
		return ""
	}
	s := "Anyone with this link can see the run."
	if b.Retention > 0 {
		s += " The service keeps it for " + days(b.Retention) + " after the last run."
	}
	return s
}

func days(d time.Duration) string {
	if d >= 48*time.Hour {
		return strconv.Itoa(int(d/(24*time.Hour))) + " days"
	}
	return strconv.Itoa(int(d/time.Hour)) + " hours"
}

// Binding returns the project this machine uses at base: the environment's
// (envBinding), else the one the service issued, else the identity made here.
func (id *Identity) Binding(base *url.URL) Binding {
	if b, ok, _ := envBinding(); ok {
		return b
	}
	if is := id.Issued[base.String()]; is != nil {
		return Binding{ProjectID: is.ProjectID, PushToken: is.PushToken, ViewKey: is.ViewKey, Hosted: true,
			Retention: time.Duration(is.RetentionSeconds) * time.Second}
	}
	return Binding{ProjectID: id.ProjectID, PushToken: id.PushToken}
}

// envBinding reads the project named by the environment. All of the project and
// the token are needed, or neither; the view key is optional (without it the
// page address cannot be shown, only pushed to).
func envBinding() (Binding, bool, error) {
	p, t, k := os.Getenv(envPrefix+"PROJECT"), os.Getenv(envPrefix+"TOKEN"), os.Getenv(envPrefix+"VIEW_KEY")
	if p == "" && t == "" && k == "" {
		return Binding{}, false, nil
	}
	if p == "" || t == "" {
		return Binding{}, false, fmt.Errorf("%sPROJECT and %sTOKEN go together; one of them is not set", envPrefix, envPrefix)
	}
	if !idPattern.MatchString(p) || !tokenPattern.MatchString(t) || (k != "" && !keyPattern.MatchString(k)) {
		return Binding{}, false, fmt.Errorf("%sPROJECT, _TOKEN or _VIEW_KEY is not in the form the service issues", envPrefix)
	}
	return Binding{ProjectID: p, PushToken: t, ViewKey: k, Hosted: true}, true, nil
}

// Resolved is the project for a run, and what happened while finding it.
type Resolved struct {
	Binding
	Identity *Identity // nil when the environment named the project
	Made     bool      // the service made a project in this call
}

// Resolve returns the project to use at base. For a service on this machine, or
// an older one, that is the identity kept here, which it creates at the first
// push. For a hosted one it is a project the service issued, asked for once.
// The error says why a live view is not possible; it never means a run must
// stop.
func Resolve(ctx context.Context, base *url.URL) (*Resolved, error) {
	if b, ok, err := envBinding(); err != nil {
		return nil, err
	} else if ok {
		return &Resolved{Binding: b}, nil
	}
	id, err := Load()
	if err != nil {
		return nil, err
	}
	made, err := id.provision(ctx, base)
	if err != nil {
		return nil, fmt.Errorf("%s://%s %w", base.Scheme, base.Host, err)
	}
	return &Resolved{Binding: id.Binding(base), Identity: id, Made: made}, nil
}

type info struct {
	Mode              string `json:"mode"`
	AcceptingProjects bool   `json:"acceptingProjects"`
	Limits            struct {
		RetentionSeconds int `json:"retentionSeconds"`
	} `json:"limits"`
}

// provision makes sure id has a project issued by the service at base, when
// the service is a hosted one. It returns whether it made one.
func (id *Identity) provision(ctx context.Context, base *url.URL) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	in, ok := fetchInfo(ctx, base)
	if !ok || in.Mode != "public" {
		return false, nil // this machine's own service, or an older one: the identity made here
	}
	key := base.String()
	if is := id.Issued[key]; is != nil {
		if !projectGone(ctx, base, is.ViewKey) {
			return false, nil
		}
		// An idle project is deleted after a while, and a deleted one cannot
		// be made again by a push. Ask for a new one.
		delete(id.Issued, key)
	}
	if !in.AcceptingProjects {
		return false, errors.New("is not taking new projects at the moment; try again later")
	}
	is, err := issue(ctx, base)
	if err != nil {
		return false, err
	}
	if in.Limits.RetentionSeconds > 0 {
		is.RetentionSeconds = in.Limits.RetentionSeconds
	}
	if id.Issued == nil {
		id.Issued = map[string]*Issued{}
	}
	id.Issued[key] = is
	if _, err := save(id.Path, id, id.Created); err != nil {
		return false, fmt.Errorf("cannot keep the project it made (%v); without it every run would be a new project", err)
	}
	return true, nil
}

var noRedirect = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

func get(ctx context.Context, u string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	return noRedirect.Do(req)
}

// fetchInfo reads what a service says about itself. A service that does not
// answer, or answers with something else, is treated as one that has no
// hosted mode.
func fetchInfo(ctx context.Context, base *url.URL) (info, bool) {
	resp, err := get(ctx, base.String()+"/v1/info")
	if err != nil {
		return info{}, false
	}
	defer resp.Body.Close()
	var in info
	if resp.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&in) != nil {
		return info{}, false
	}
	return in, true
}

// projectGone reports whether the service has no project for the view key: a
// 404, and only that. A service that is busy or unreachable says nothing about
// the project, so it is kept.
func projectGone(ctx context.Context, base *url.URL, viewKey string) bool {
	resp, err := get(ctx, base.String()+"/api/projects/"+viewKey)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusNotFound
}

// issue asks the service for a new project.
func issue(ctx context.Context, base *url.URL) (*Issued, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base.String()+"/v1/projects", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := noRedirect.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return nil, fmt.Errorf("could not ask for a project (%v)", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode != http.StatusCreated {
		return nil, refused(resp, body)
	}
	var got struct {
		ID        string `json:"id"`
		PushToken string `json:"pushToken"`
		ViewKey   string `json:"viewKey"`
	}
	if json.Unmarshal(body, &got) != nil {
		return nil, errors.New("answered with a project that cannot be read")
	}
	// Only the three secrets are used. The addresses in the answer are not: a
	// push address chosen by the service could point somewhere else, and the
	// token is sent to whatever address is used.
	is := &Issued{ProjectID: got.ID, PushToken: got.PushToken, ViewKey: got.ViewKey, CreatedAt: time.Now().UTC()}
	if !is.valid() {
		return nil, errors.New("answered with a project in a form this version does not accept")
	}
	return is, nil
}

// refused words a service's "no" for a person. It names the status and the
// service's own code, and how long to wait when it said so.
func refused(resp *http.Response, body []byte) error {
	var e struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(body, &e)
	what := "answered " + strconv.Itoa(resp.StatusCode)
	if e.Code != "" && len(e.Code) <= 40 && !strings.ContainsAny(e.Code, " \n\r\t") {
		what += " " + e.Code
	}
	if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && s > 0 {
		what += "; try again in " + (time.Duration(s) * time.Second).String()
	}
	return errors.New(what + " when asked for a project")
}
