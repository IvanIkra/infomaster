package telegram

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/auth/qrlogin"
	"github.com/gotd/td/tg"
	"rsc.io/qr"

	"github.com/IvanIkra/infomaster/go/internal/config"
)

// LoginQR logs the twin in from a terminal by a QR code (`infomaster login --qr`): the twin's app scans it under
// Settings → Devices → Link Desktop Device, so no login code is needed — for when Telegram says it sent a code and
// none arrives. Then the cloud password, if the account has one. Talks to Telegram; nothing of it runs in the tests.
func LoginQR(ctx context.Context, env config.Env, sessionPath string, in io.Reader, out io.Writer) error {
	reader := bufio.NewReader(in)
	d := tg.NewUpdateDispatcher()
	loggedIn := qrlogin.OnLoginToken(d)
	options := Options(sessionPath)
	options.UpdateHandler = d
	client := telegram.NewClient(int(env.APIID), env.APIHash, options)
	return client.Run(ctx, func(ctx context.Context) error {
		me, err := client.Self(ctx)
		if err != nil { // not logged in yet
			login := qrlogin.NewQR(client.API(), int(env.APIID), env.APIHash, qrlogin.Options{
				Migrate: client.MigrateTo,
				Password: func(context.Context) (string, error) {
					fmt.Fprint(out, "Please enter your password: ")
					line, err := reader.ReadString('\n')
					if err != nil && (err != io.EOF || line == "") {
						return "", err
					}
					return strings.TrimSpace(line), nil
				},
			})
			_, err := login.Auth(ctx, loggedIn, func(_ context.Context, token qrlogin.Token) error {
				code, err := qr.Encode(token.URL(), qr.L)
				if err != nil {
					return err
				}
				fmt.Fprintln(out, "\nIn the twin's Telegram: Settings → Devices → Link Desktop Device, and scan:")
				fmt.Fprint(out, terminalQR(code))
				fmt.Fprintf(out, "(a new code comes by itself before this one expires at %s)\n",
					token.Expires().Local().Format("15:04:05"))
				return nil
			})
			if err != nil {
				return err
			}
			if me, err = client.Self(ctx); err != nil {
				return err
			}
		}
		if me.Bot || me.ID == env.OwnerID {
			// The twin is its own account (D15): not the bot, not the owner's.
			_, _ = client.API().AuthLogOut(ctx)
			_ = os.Remove(sessionPath)
			return errors.New("this is no twin: a separate account is needed, not a bot and not TG_OWNER_ID")
		}
		if err := os.Chmod(sessionPath, 0o600); err != nil {
			return err
		}
		fmt.Fprintf(out, "The twin logged in: %s (id %d). Restart the service.\n", DisplayName(me), me.ID)
		return nil
	})
}

// terminalQR draws the code with half blocks, two rows of modules a line, dark on light with a quiet zone of 2.
func terminalQR(code *qr.Code) string {
	const quiet = 2
	dark := func(x, y int) bool {
		return x >= 0 && y >= 0 && x < code.Size && y < code.Size && code.Black(x, y)
	}
	var b strings.Builder
	for y := -quiet; y < code.Size+quiet; y += 2 {
		for x := -quiet; x < code.Size+quiet; x++ {
			top, bottom := dark(x, y), dark(x, y+1)
			switch {
			case top && bottom:
				b.WriteString(" ")
			case top:
				b.WriteString("▄")
			case bottom:
				b.WriteString("▀")
			default:
				b.WriteString("█")
			}
		}
		b.WriteString("\n")
	}
	return b.String()
}
