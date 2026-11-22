// Package config reads secrets and addresses from the environment (§10). User settings live in the database.
package config

import (
	"bufio"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// Error is a configuration error.
type Error struct{ Msg string }

func (e *Error) Error() string { return e.Msg }

// LoadEnv reads KEY=VALUE lines from a .env file; real environment variables win.
func LoadEnv(path string) error {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") || !strings.Contains(line, "=") {
			continue
		}
		key, value, _ := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		if _, set := os.LookupEnv(key); !set {
			os.Setenv(key, strings.Trim(strings.TrimSpace(value), `'"`))
		}
	}
	return scanner.Err()
}

// Env is the service's environment.
type Env struct {
	APIID                int64
	APIHash              string
	BotToken             string
	OwnerID              int64
	APIToken             string
	PublicURL            string // Funnel address of the public port, no trailing slash
	PublicPort           int
	InternalPort         int
	RuzICalURL           string // optional: without it the timetable has no RUZ classes
	CalendarSecret       string // optional: without it the iCal feeds of the internal port are off
	CalendarPublicSecret string // optional: without it no feeds behind Funnel
	MailUser             string // optional, with the password: without both the mail source is off
	MailPassword         string
	MailHost             string
	MailPort             int
}

var (
	httpLink = regexp.MustCompile(`^https?://`)
	secretRe = regexp.MustCompile(`^[A-Za-z0-9_-]{16,}$`)
)

// FromEnviron reads Env from a getter such as os.Getenv.
func FromEnviron(get func(string) string) (Env, error) {
	env := Env{PublicPort: 8080, InternalPort: 8081, MailHost: "imap.yandex.ru", MailPort: 993}
	val := func(name string) string { return strings.TrimSpace(get(name)) }
	var missing []string
	for _, name := range []string{"TG_API_ID", "TG_API_HASH", "TG_BOT_TOKEN", "TG_OWNER_ID", "API_TOKEN", "PUBLIC_URL"} {
		if val(name) == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return env, &Error{"not set in the environment or .env: " + strings.Join(missing, ", ")}
	}
	integer := func(name, field string, out any) error {
		text := val(name)
		if text == "" {
			return nil
		}
		n, err := strconv.ParseInt(text, 10, 64)
		if err != nil {
			return &Error{fmt.Sprintf("%s must be an integer, got '%s'", field, text)}
		}
		switch p := out.(type) {
		case *int64:
			*p = n
		case *int:
			*p = int(n)
		}
		return nil
	}
	for _, f := range []struct {
		name, field string
		out         any
	}{
		{"TG_API_ID", "api_id", &env.APIID}, {"TG_OWNER_ID", "owner_id", &env.OwnerID},
		{"HTTP_PUBLIC_PORT", "public_port", &env.PublicPort}, {"HTTP_INTERNAL_PORT", "internal_port", &env.InternalPort},
		{"MAIL_PORT", "mail_port", &env.MailPort},
	} {
		if err := integer(f.name, f.field, f.out); err != nil {
			return env, err
		}
	}
	env.APIHash, env.BotToken, env.APIToken = val("TG_API_HASH"), val("TG_BOT_TOKEN"), val("API_TOKEN")
	env.MailUser, env.MailPassword = val("MAIL_USER"), val("MAIL_PASSWORD")
	if host := val("MAIL_HOST"); host != "" {
		env.MailHost = host
	}
	env.PublicURL = val("PUBLIC_URL")
	if !strings.HasPrefix(env.PublicURL, "https://") {
		return env, &Error{"PUBLIC_URL must be the https:// Funnel address"}
	}
	env.PublicURL = strings.TrimRight(env.PublicURL, "/")
	if ruz := val("RUZ_ICAL_URL"); ruz != "" {
		if !httpLink.MatchString(ruz) {
			return env, &Error{"RUZ_ICAL_URL must be an http(s):// link"}
		}
		env.RuzICalURL = ruz
	}
	for _, s := range []struct {
		name string
		out  *string
	}{{"CALENDAR_SECRET", &env.CalendarSecret}, {"CALENDAR_PUBLIC_SECRET", &env.CalendarPublicSecret}} {
		if secret := val(s.name); secret != "" {
			// A path segment that cannot be guessed: calendars send no headers, the secret is all the protection.
			if !secretRe.MatchString(secret) {
				return env, &Error{s.name + " must be at least 16 characters of A-Z, a-z, 0-9, _ and -"}
			}
			*s.out = secret
		}
	}
	if env.CalendarPublicSecret != "" && env.CalendarPublicSecret == env.CalendarSecret {
		return env, &Error{"CALENDAR_PUBLIC_SECRET must differ from CALENDAR_SECRET"}
	}
	return env, nil
}
