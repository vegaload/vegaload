package rabbitmq

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/vegaload/vegaload/internal/urlerr"
)

func (d *Driver) parseURL() error {
	u, err := url.Parse(d.target.URL)
	if err != nil {
		return fmt.Errorf("rabbitmq: parsing target URL: %v", urlerr.Inner(err))
	}
	switch u.Scheme {
	case "amqp":
		d.port = "5672"
	case "amqps":
		d.port = "5671"
		d.useTLS = true
	default:
		return fmt.Errorf("rabbitmq: unsupported scheme %q, want amqp:// or amqps://", u.Scheme)
	}
	if u.Hostname() == "" {
		return fmt.Errorf("rabbitmq: the target has no host")
	}
	d.host = u.Hostname()
	if u.Port() != "" {
		d.port = u.Port()
	}
	if u.User != nil {
		if _, ok := u.User.Password(); ok {
			return fmt.Errorf("rabbitmq: do not put a password in the URL, use -opt password_env=NAME")
		}
		d.username = u.User.Username()
	}
	if q := u.Query(); len(q) > 0 {
		keys := make([]string, 0, len(q))
		for k := range q {
			keys = append(keys, k)
		}
		// Sorted so the error is stable. The values are never printed.
		sortStrings(keys)
		return fmt.Errorf("rabbitmq: the URL query is not allowed (%s)", strings.Join(keys, ", "))
	}
	vhost, given, err := vhostFromPath(u.EscapedPath())
	if err != nil {
		return err
	}
	d.vhost = vhost
	d.urlVhost = vhost
	d.urlVhostGiven = given
	return nil
}

func vhostFromPath(path string) (string, bool, error) {
	if path == "" || path == "/" {
		return "/", false, nil
	}
	if !strings.HasPrefix(path, "/") {
		return "", false, fmt.Errorf("rabbitmq: the vhost path %q is not valid", path)
	}
	raw := path[1:]
	dec, err := url.PathUnescape(raw)
	if err != nil {
		return "", false, fmt.Errorf("rabbitmq: the vhost path is not valid")
	}
	if dec == "" {
		return "/", false, nil
	}
	return dec, true, nil
}

func (d *Driver) readConn(passwordGiven bool) error {
	opts := d.target.Options
	d.insecure = d.target.InsecureSkipVerify
	urlUser := d.username
	if v, ok := opts["username"]; ok {
		if urlUser != "" && urlUser != v {
			return fmt.Errorf("rabbitmq: username is set in the URL and in the options, and they differ")
		}
		d.username = v
	}
	if d.username == "" {
		d.username = "guest"
	}
	if v, ok := opts["vhost"]; ok {
		if d.urlVhostGiven && d.urlVhost != v {
			return fmt.Errorf("rabbitmq: vhost is set in the URL and in the options, and they differ")
		}
		d.vhost = v
	}
	if !passwordGiven {
		if name, ok := opts["password_env"]; ok {
			v, exists := os.LookupEnv(name)
			if !exists {
				return fmt.Errorf("rabbitmq: password_env %s is not set", name)
			}
			d.password = v
			passwordGiven = true
		} else if d.username == "guest" {
			d.password = "guest"
			passwordGiven = true
		} else if d.script {
			return fmt.Errorf("rabbitmq: username %q needs a password: pass password: env.NAME", d.username)
		} else {
			return fmt.Errorf("rabbitmq: username %q needs password_env", d.username)
		}
	}
	if v, ok := opts["tls"]; ok {
		switch v {
		case "false":
			if strings.HasPrefix(d.target.URL, "amqps:") {
				return fmt.Errorf("rabbitmq: tls=false is not allowed with amqps://")
			}
			d.useTLS = false
			d.skipVerify = false
		case "true":
			d.useTLS = true
			d.skipVerify = d.insecure
		case "skip-verify":
			d.useTLS = true
			d.skipVerify = true
		default:
			return fmt.Errorf("rabbitmq: tls=%q, want false, true, or skip-verify", v)
		}
	} else if d.useTLS && d.insecure {
		d.skipVerify = true
	}
	if v, ok := opts["connection"]; ok {
		switch v {
		case "shared":
			d.perCall = false
		case "per_call":
			d.perCall = true
		default:
			return fmt.Errorf("rabbitmq: connection=%q, want shared or per_call", v)
		}
	}
	n, err := d.target.OptionInt("channels", 10)
	if err != nil {
		return fmt.Errorf("rabbitmq: %w", err)
	}
	if n < 1 || n > 1000 {
		return fmt.Errorf("rabbitmq: channels must be from 1 to 1000")
	}
	d.channels = n
	if v, ok := opts["heartbeat"]; ok {
		if v == "0" {
			d.heartbeat = 0
		} else {
			hb, err := time.ParseDuration(v)
			if err != nil {
				return fmt.Errorf("rabbitmq: option heartbeat=%q: want a duration such as 10s, or 0", v)
			}
			if hb < time.Second || hb > 5*time.Minute {
				return fmt.Errorf("rabbitmq: heartbeat must be 0 or from 1s to 5m")
			}
			d.heartbeat = hb
		}
	}
	if v, ok := opts["connection_name"]; ok && v != "" {
		d.connName = v
	}
	d.allowWrites, err = d.target.OptionBool("allow_writes", false)
	if err != nil {
		return fmt.Errorf("rabbitmq: %w", err)
	}
	d.allowAdmin, err = d.target.OptionBool("allow_admin", false)
	if err != nil {
		return fmt.Errorf("rabbitmq: %w", err)
	}
	if d.allowAdmin && !d.allowWrites {
		return fmt.Errorf("rabbitmq: allow_admin=true needs allow_writes=true as well")
	}
	_ = passwordGiven
	return nil
}

func (d *Driver) readJob() error {
	if len(d.target.Body) > maxBody {
		return fmt.Errorf("rabbitmq: the body is %d bytes, the most is %d", len(d.target.Body), maxBody)
	}
	d.mode = d.target.Option("mode", modePublish)
	switch d.mode {
	case modePublish, modeConsume, modeRoundtrip, modeAdmin:
	default:
		return fmt.Errorf("rabbitmq: mode=%q, want publish, consume, roundtrip, or admin", d.mode)
	}
	if err := d.checkModeOptions(); err != nil {
		return err
	}
	d.exchange = d.target.Option("exchange", "")
	d.routingKey = d.target.Option("routing_key", "")
	d.queue = d.target.Option("queue", "")
	d.bindKey = d.target.Option("bind_key", "")
	var err error
	d.count, err = d.target.OptionInt("count", 1)
	if err != nil {
		return fmt.Errorf("rabbitmq: %w", err)
	}
	if d.count < 1 || d.count > maxCount {
		return fmt.Errorf("rabbitmq: count must be from 1 to %d", maxCount)
	}
	d.confirm, err = d.target.OptionBool("confirm", true)
	if err != nil {
		return fmt.Errorf("rabbitmq: %w", err)
	}
	defMandatory := d.confirm
	if d.mode == modeRoundtrip {
		defMandatory = true
	}
	d.mandatory, err = d.target.OptionBool("mandatory", defMandatory)
	if err != nil {
		return fmt.Errorf("rabbitmq: %w", err)
	}
	if d.mode == modePublish && d.mandatory && !d.confirm {
		return fmt.Errorf("rabbitmq: mandatory=true needs confirm=true")
	}
	if d.mode == modeRoundtrip && !d.mandatory {
		return fmt.Errorf("rabbitmq: roundtrip always uses mandatory=true")
	}
	d.persistent, err = d.target.OptionBool("persistent", false)
	if err != nil {
		return fmt.Errorf("rabbitmq: %w", err)
	}
	d.contentType = d.target.Option("content_type", "")
	if _, ok := d.target.Options["priority"]; ok {
		d.priority, err = d.target.OptionInt("priority", 0)
		if err != nil {
			return fmt.Errorf("rabbitmq: %w", err)
		}
		if d.priority < 0 || d.priority > 255 {
			return fmt.Errorf("rabbitmq: priority must be from 0 to 255")
		}
		d.prioritySet = true
	}
	if v, ok := d.target.Options["expiration"]; ok {
		exp, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("rabbitmq: option expiration=%q: want a duration such as 30s", v)
		}
		if exp < time.Millisecond {
			return fmt.Errorf("rabbitmq: expiration must be at least 1ms")
		}
		d.expiration = strconv.FormatInt(exp.Milliseconds(), 10)
	}
	if v, ok := d.target.Options["headers"]; ok {
		d.headers, err = parseHeaders(v)
		if err != nil {
			return err
		}
	}
	d.ack = d.target.Option("ack", "requeue")
	if d.ack != "requeue" && d.ack != "ack" {
		return fmt.Errorf("rabbitmq: ack=%q, want requeue or ack", d.ack)
	}
	defPrefetch := d.count
	if defPrefetch > 1000 {
		defPrefetch = 1000
	}
	d.prefetch, err = d.target.OptionInt("prefetch", defPrefetch)
	if err != nil {
		return fmt.Errorf("rabbitmq: %w", err)
	}
	if _, ok := d.target.Options["prefetch"]; ok && d.prefetch == 0 {
		return fmt.Errorf("rabbitmq: prefetch 0 means no limit and is not allowed")
	}
	if d.prefetch < 1 || d.prefetch > 65535 {
		return fmt.Errorf("rabbitmq: prefetch must be from 1 to 65535")
	}
	d.expect = d.target.Option("expect", "")
	d.action = d.target.Option("action", "")
	d.durable, err = d.target.OptionBool("durable", true)
	if err != nil {
		return fmt.Errorf("rabbitmq: %w", err)
	}
	d.autoDelete, err = d.target.OptionBool("auto_delete", false)
	if err != nil {
		return fmt.Errorf("rabbitmq: %w", err)
	}
	d.queueType = d.target.Option("queue_type", "")
	if d.queueType != "" && d.queueType != "classic" && d.queueType != "quorum" && d.queueType != "stream" {
		return fmt.Errorf("rabbitmq: queue_type=%q, want classic, quorum, or stream", d.queueType)
	}
	d.exchangeType = d.target.Option("exchange_type", "direct")
	if d.exchangeType == "headers" {
		return fmt.Errorf("rabbitmq: exchange type headers is not supported")
	}
	if d.exchangeType != "direct" && d.exchangeType != "fanout" && d.exchangeType != "topic" {
		return fmt.Errorf("rabbitmq: exchange_type=%q, want direct, fanout, or topic", d.exchangeType)
	}
	if d.mode == modeRoundtrip && d.exchange == "" {
		if _, ok := d.target.Options["routing_key"]; ok {
			return fmt.Errorf("rabbitmq: option routing_key is not used in mode roundtrip with the default exchange")
		}
		if _, ok := d.target.Options["bind_key"]; ok {
			return fmt.Errorf("rabbitmq: option bind_key needs a named exchange")
		}
	}
	if d.mode == modeRoundtrip && d.exchange != "" {
		if _, ok := d.target.Options["bind_key"]; !ok {
			d.bindKey = d.routingKey
		}
	}
	switch d.mode {
	case modeConsume:
		if d.queue == "" {
			return fmt.Errorf("rabbitmq: consume needs a queue")
		}
	case modeAdmin:
		if d.action == "" {
			return fmt.Errorf("rabbitmq: admin needs an action")
		}
		switch d.action {
		case "queue_info", "queue_declare", "queue_delete", "queue_purge", "queue_lifecycle":
			if d.queue == "" {
				return fmt.Errorf("rabbitmq: action %s needs a queue", d.action)
			}
		case "exchange_declare", "exchange_delete":
			if d.exchange == "" {
				return fmt.Errorf("rabbitmq: action %s needs an exchange", d.action)
			}
		default:
			return fmt.Errorf("rabbitmq: action=%q is not known", d.action)
		}
		if d.action == "queue_lifecycle" && !strings.Contains(d.queue, "{id}") {
			return fmt.Errorf("rabbitmq: queue_lifecycle needs {id} in the queue name, so it never deletes a queue that was already there")
		}
	}
	return nil
}

func (d *Driver) checkModeOptions() error {
	allowed := map[string]bool{
		"username": true, "password_env": true, "vhost": true, "tls": true,
		"connection": true, "channels": true, "heartbeat": true,
		"connection_name": true, "allow_writes": true, "allow_admin": true,
		"mode": true,
	}
	var job []string
	switch d.mode {
	case modePublish:
		job = []string{"exchange", "routing_key", "count", "confirm", "mandatory", "persistent", "content_type", "priority", "expiration", "headers"}
	case modeConsume:
		job = []string{"queue", "count", "ack", "prefetch", "expect"}
	case modeRoundtrip:
		job = []string{"exchange", "routing_key", "bind_key", "count", "mandatory", "persistent", "content_type", "priority", "expiration", "headers", "expect"}
	case modeAdmin:
		job = []string{"action", "queue", "exchange", "durable", "auto_delete", "queue_type", "exchange_type"}
	}
	for _, k := range job {
		allowed[k] = true
	}
	var extra []string
	for k := range d.target.Options {
		if !allowed[k] {
			extra = append(extra, k)
		}
	}
	sortStrings(extra)
	for _, k := range extra {
		if k == "queue" && (d.mode == modePublish || d.mode == modeRoundtrip) {
			return fmt.Errorf("rabbitmq: option queue is not used in mode %s, use routing_key", d.mode)
		}
		return fmt.Errorf("rabbitmq: option %s is not used in mode %s", k, d.mode)
	}
	return nil
}

func parseHeaders(s string) (amqp.Table, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(s), &raw); err != nil {
		return nil, fmt.Errorf("rabbitmq: option headers must be a JSON object")
	}
	out := amqp.Table{}
	for k, v := range raw {
		val, err := headerValue(v)
		if err != nil {
			return nil, err
		}
		out[k] = val
	}
	return out, nil
}

func headerValue(v json.RawMessage) (any, error) {
	text := strings.TrimSpace(string(v))
	if text == "true" || text == "false" {
		return text == "true", nil
	}
	if strings.HasPrefix(text, "\"") {
		var s string
		if err := json.Unmarshal(v, &s); err != nil {
			return nil, fmt.Errorf("rabbitmq: option headers values must be a string, number, or bool")
		}
		return s, nil
	}
	if strings.HasPrefix(text, "{") || strings.HasPrefix(text, "[") {
		return nil, fmt.Errorf("rabbitmq: option headers values must be a string, number, or bool")
	}
	var n json.Number
	dec := json.NewDecoder(strings.NewReader(text))
	dec.UseNumber()
	if err := dec.Decode(&n); err != nil {
		return nil, fmt.Errorf("rabbitmq: option headers values must be a string, number, or bool")
	}
	if i, err := n.Int64(); err == nil {
		if i >= -1<<31 && i <= 1<<31-1 {
			return int32(i), nil
		}
		return i, nil
	}
	f, err := n.Float64()
	if err != nil {
		return nil, fmt.Errorf("rabbitmq: option headers values must be a string, number, or bool")
	}
	return f, nil
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		j := i
		for j > 0 && s[j] < s[j-1] {
			s[j], s[j-1] = s[j-1], s[j]
			j--
		}
	}
}
