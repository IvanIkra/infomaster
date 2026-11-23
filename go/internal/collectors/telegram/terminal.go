package telegram

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/tgerr"
	"golang.org/x/term"

	"github.com/IvanIkra/infomaster/go/internal/config"
)

// The one-time login from a terminal: phone, code, the cloud password if there is one.

// Login logs the twin in from a terminal (`infomaster login`); dial is DialGotd outside the tests.
func Login(ctx context.Context, env config.Env, sessionPath string, in io.Reader, out io.Writer,
	dial func(ctx context.Context, env config.Env, sessionPath string) (LoginClient, error)) error {
	client, err := dial(ctx, env, sessionPath)
	if err != nil {
		return err
	}
	defer client.Close()
	reader := bufio.NewReader(in)
	ask := func(prompt string) (string, error) {
		fmt.Fprint(out, prompt)
		line, err := reader.ReadString('\n')
		if err != nil && (err != io.EOF || line == "") {
			return "", err
		}
		return strings.TrimSpace(line), nil
	}
	me, err := client.Self(ctx)
	if err != nil { // not logged in yet
		phone, err := ask("Please enter your phone: ")
		if err != nil {
			return err
		}
		phone = PhoneNumber(phone)
		hash, err := client.SendCode(ctx, phone)
		if err != nil {
			return err
		}
		for {
			code, err := ask("Please enter the code you received: ")
			if err != nil {
				return err
			}
			err = client.SignIn(ctx, phone, code, hash)
			if errors.Is(err, auth.ErrPasswordAuthNeeded) {
				for {
					password, err := secret(in, reader, out, "Please enter your password: ")
					if err != nil {
						return err
					}
					err = client.Password(ctx, password)
					if errors.Is(err, auth.ErrPasswordInvalid) {
						fmt.Fprintln(out, "Invalid password. Please try again")
						continue
					}
					if err != nil {
						return err
					}
					break
				}
				break
			}
			if err != nil && tgerr.Is(err, "PHONE_CODE_INVALID") {
				fmt.Fprintln(out, "Invalid code. Please try again.")
				continue
			}
			if err != nil {
				return err
			}
			break
		}
		if me, err = client.Self(ctx); err != nil {
			return err
		}
	}
	if me.Bot || me.ID == env.OwnerID {
		// The twin is its own account (D15): not the bot, not the owner's.
		_ = client.LogOut(ctx)
		_ = os.Remove(sessionPath)
		return errors.New("this is no twin: a separate account is needed, not a bot and not TG_OWNER_ID")
	}
	if err := os.Chmod(sessionPath, 0o600); err != nil {
		return err
	}
	fmt.Fprintf(out, "The twin logged in: %s (id %d). Restart the service.\n", DisplayName(me), me.ID)
	return nil
}

// secret reads the cloud password: without echo from a terminal, so it stays off the screen and out of its
// scrollback; a line of in otherwise (the tests, a pipe).
func secret(in io.Reader, reader *bufio.Reader, out io.Writer, prompt string) (string, error) {
	fmt.Fprint(out, prompt)
	if f, ok := in.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		b, err := term.ReadPassword(int(f.Fd()))
		fmt.Fprintln(out)
		return strings.TrimSpace(string(b)), err
	}
	line, err := reader.ReadString('\n')
	if err != nil && (err != io.EOF || line == "") {
		return "", err
	}
	return strings.TrimSpace(line), nil
}
