package ftp

import (
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/vegaload/vegaload/internal/protocol/xfercommon"
	"github.com/vegaload/vegaload/internal/urlerr"
)

func (d *Driver) parseURL() error {
	u, err := url.Parse(d.target.URL)
	if err != nil {
		return fmt.Errorf("ftp: parsing target URL: %v", urlerr.Inner(err))
	}
	switch strings.ToLower(u.Scheme) {
	case "ftp":
		d.port = "21"
		d.tlsMode = "none"
	case "ftps":
		d.port = "990"
		d.tlsMode = "implicit"
	default:
		return fmt.Errorf("ftp: unsupported scheme %q, want ftp:// or ftps://", u.Scheme)
	}
	if u.Hostname() == "" {
		return fmt.Errorf("ftp: the target has no host")
	}
	d.host = u.Hostname()
	if u.Port() != "" {
		d.port = u.Port()
	}
	if u.User != nil {
		if _, ok := u.User.Password(); ok {
			return fmt.Errorf("ftp: do not put a password in the URL, use -opt password_env=NAME")
		}
		d.username = u.User.Username()
	}
	if q := u.Query(); len(q) > 0 {
		keys := make([]string, 0, len(q))
		for k := range q {
			keys = append(keys, k)
		}
		sortStrings(keys)
		return fmt.Errorf("ftp: the URL query is not allowed (%s)", strings.Join(keys, ", "))
	}
	if u.EscapedPath() != "" && u.EscapedPath() != "/" {
		return fmt.Errorf("ftp: the URL path is not allowed")
	}
	return nil
}

func (d *Driver) readConn(password *string) error {
	opts := d.target.Options
	if err := xfercommon.CheckText("username", d.username); err != nil {
		return fmt.Errorf("ftp: %w", err)
	}
	urlUser := d.username
	if v, ok := opts["username"]; ok {
		if err := xfercommon.CheckText("username", v); err != nil {
			return fmt.Errorf("ftp: %w", err)
		}
		if urlUser != "" && urlUser != v {
			return fmt.Errorf("ftp: username is set in the URL and in the options, and they differ")
		}
		d.username = v
	}
	if password != nil {
		if err := xfercommon.CheckText("password", *password); err != nil {
			return fmt.Errorf("ftp: %w", err)
		}
		d.password = *password
	} else if name, ok := opts["password_env"]; ok {
		if d.username == "" {
			return fmt.Errorf("ftp: password_env needs a username")
		}
		v, exists := os.LookupEnv(name)
		if !exists {
			return fmt.Errorf("ftp: password_env %s is not set", name)
		}
		if err := xfercommon.CheckText("password", v); err != nil {
			return fmt.Errorf("ftp: %w", err)
		}
		d.password = v
	} else if d.username == "" || d.username == "anonymous" {
		d.username = "anonymous"
		d.password = "vegaload@"
	} else if d.script {
		return fmt.Errorf("ftp: username %q needs a password: pass password: env.NAME", d.username)
	} else {
		return fmt.Errorf("ftp: username %q needs password_env", d.username)
	}

	if v, ok := opts["tls"]; ok {
		switch v {
		case "none":
			if strings.HasPrefix(strings.ToLower(d.target.URL), "ftps:") {
				return fmt.Errorf("ftp: tls=none is not allowed with ftps://")
			}
			d.tlsMode = "none"
		case "explicit":
			if strings.HasPrefix(strings.ToLower(d.target.URL), "ftps:") {
				return fmt.Errorf("ftp: tls=explicit is not allowed with ftps://")
			}
			d.tlsMode = "explicit"
		case "implicit":
			d.tlsMode = "implicit"
		default:
			return fmt.Errorf("ftp: tls=%q, want none, explicit, or implicit", v)
		}
	}
	verify, err := d.target.OptionBool("tls_verify", true)
	if err != nil {
		return fmt.Errorf("ftp: %w", err)
	}
	d.tlsVerify = verify && !d.target.InsecureSkipVerify
	if d.tlsMode == "none" {
		d.tlsVerify = true
	}

	if v, ok := opts["connection"]; ok {
		switch v {
		case "shared":
			d.perCall = false
		case "per_call":
			d.perCall = true
		default:
			return fmt.Errorf("ftp: connection=%q, want shared or per_call", v)
		}
	}
	n, err := d.target.OptionInt("sessions", 10)
	if err != nil {
		return fmt.Errorf("ftp: %w", err)
	}
	if n < 1 || n > 10000 {
		return fmt.Errorf("ftp: sessions must be from 1 to 10000")
	}
	d.sessions = n

	switch d.target.Option("data_host", "control") {
	case "control":
		d.dataHostControl = true
	case "announced":
		d.dataHostControl = false
	default:
		return fmt.Errorf("ftp: data_host=%q, want control or announced", d.target.Option("data_host", "control"))
	}
	switch d.target.Option("epsv", "auto") {
	case "auto":
		d.epsvOff = false
	case "off":
		d.epsvOff = true
	default:
		return fmt.Errorf("ftp: epsv=%q, want auto or off", d.target.Option("epsv", "auto"))
	}

	d.allowWrites, err = d.target.OptionBool("allow_writes", false)
	if err != nil {
		return fmt.Errorf("ftp: %w", err)
	}
	d.allowAdmin, err = d.target.OptionBool("allow_admin", false)
	if err != nil {
		return fmt.Errorf("ftp: %w", err)
	}
	if d.allowAdmin && !d.allowWrites {
		return fmt.Errorf("ftp: allow_admin=true needs allow_writes=true as well")
	}
	return nil
}

func (d *Driver) readJob() error {
	if len(d.target.Body) > xfercommon.MaxBody {
		return fmt.Errorf("ftp: the body is %d bytes, the most is %d", len(d.target.Body), xfercommon.MaxBody)
	}
	d.mode = d.target.Option("mode", modeDownload)
	switch d.mode {
	case modeConnect, modeDownload, modeUpload, modeList, modeStat, modeDelete, modeRoundtrip:
	default:
		return fmt.Errorf("ftp: mode=%q, want connect, download, upload, list, stat, delete, or roundtrip", d.mode)
	}
	if err := d.rejectForeignOptions(); err != nil {
		return err
	}
	d.path = d.target.Option("path", "")
	if d.mode == modeList && d.path == "" {
		d.path = "."
	}
	if d.mode != modeConnect {
		if err := xfercommon.CheckPath(d.path); err != nil {
			return fmt.Errorf("ftp: %w", err)
		}
	}
	if d.mode == modeList && strings.HasPrefix(d.path, "-") {
		return fmt.Errorf("ftp: a list path must not start with -")
	}
	if _, ok := d.target.Options["size"]; ok {
		n, err := xfercommon.ParseSize(d.target.Options["size"])
		if err != nil {
			return fmt.Errorf("ftp: %w", err)
		}
		d.size = n
		d.sizeSet = true
	}
	d.fill = d.target.Option("fill", "random")
	switch d.fill {
	case "random", "zero", "text":
	default:
		return fmt.Errorf("ftp: fill=%q, want random, zero, or text", d.fill)
	}
	if _, ok := d.target.Options["expect_size"]; ok {
		n, err := xfercommon.ParseSize(d.target.Options["expect_size"])
		if err != nil {
			return fmt.Errorf("ftp: %w", err)
		}
		d.expectSize = n
		d.expectSizeSet = true
	}
	d.expectSHA = d.target.Option("expect_sha256", "")
	if d.expectSHA != "" && !xfercommon.ValidSHA256(d.expectSHA) {
		return fmt.Errorf("ftp: expect_sha256 must be 64 hex characters")
	}
	d.expect = d.target.Option("expect", "")
	limit, err := d.target.OptionInt("limit", 1000)
	if err != nil {
		return fmt.Errorf("ftp: %w", err)
	}
	if limit < 1 || limit > 100000 {
		return fmt.Errorf("ftp: limit must be from 1 to 100000")
	}
	d.limit = limit
	d.hidden, err = d.target.OptionBool("hidden", false)
	if err != nil {
		return fmt.Errorf("ftp: %w", err)
	}
	if d.sizeSet && len(d.target.Body) > 0 {
		return fmt.Errorf("ftp: use size or a body, not both")
	}
	if (d.mode == modeUpload || d.mode == modeRoundtrip) && !d.sizeSet && len(d.target.Body) == 0 {
		return fmt.Errorf("ftp: upload needs size or a body")
	}
	return nil
}

// rejectForeignOptions refuses a job option that the mode does not use.
// Connection options are checked in readConn.
func (d *Driver) rejectForeignOptions() error {
	bodyModes := map[string]bool{modeUpload: true, modeRoundtrip: true}
	allowed := map[string]bool{"mode": true}
	add := func(keys ...string) {
		for _, k := range keys {
			allowed[k] = true
		}
	}
	switch d.mode {
	case modeConnect:
	case modeDownload:
		add("path", "expect_size", "expect_sha256", "expect")
	case modeUpload, modeRoundtrip:
		add("path", "size", "fill")
	case modeList:
		add("path", "expect", "limit", "hidden")
	case modeStat, modeDelete:
		add("path")
	}
	for k := range d.target.Options {
		if connOption(k) {
			continue
		}
		if !allowed[k] {
			return fmt.Errorf("ftp: option %s is not used in mode %s", k, d.mode)
		}
	}
	if len(d.target.Body) > 0 && !bodyModes[d.mode] {
		return fmt.Errorf("ftp: a body is not used in mode %s", d.mode)
	}
	return nil
}

func connOption(k string) bool {
	switch k {
	case "username", "password_env", "tls", "tls_verify", "sessions", "connection",
		"data_host", "epsv", "allow_writes", "allow_admin":
		return true
	}
	return false
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
