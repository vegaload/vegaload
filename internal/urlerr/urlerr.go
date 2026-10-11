// Package urlerr keeps a password out of an error about a URL.
//
// The error from url.Parse repeats the whole URL, password included. A
// target URL such as redis://user:secret@host can reach a log, a report or an
// MCP reply that way.
package urlerr

import (
	"errors"
	"net/url"
	"regexp"
)

// Inner returns err without the URL that url.Parse put in it. Any other error
// is returned as it is.
func Inner(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) && ue.Err != nil {
		return ue.Err
	}
	return err
}

// userinfo finds user:password@ at the start of the text or after "://". The
// password runs to the last @ before a slash, so a password that holds an
// unescaped @ is hidden whole.
var userinfo = regexp.MustCompile(`(^|://)([^/@\s:]*):[^/\s]*@`)

// Mask hides the password of a user:password@ part in raw, so the text can
// go into a message about that URL. It also handles a bare user:password@host:port.
func Mask(raw string) string {
	return userinfo.ReplaceAllString(raw, "$1$2:***@")
}
