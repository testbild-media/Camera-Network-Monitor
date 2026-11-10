package dahua

import (
	"context"
	"strings"
	"time"

	"cnm/internal/provider"
)

// The recorder's users and their passwords (userManager). The stored form
// of a password is MD5("user:realm:password") – the first step of the
// "Default" login. CNM changes passwords only on devices that log in that
// way; a change is checked with a second, separate login, and the old
// password is restored over the still open session when that login fails.

// Users lists the recorder's user accounts (no passwords).
func (d *rpcDriver) Users(ctx context.Context) ([]provider.DeviceUser, error) {
	r, err := d.rpc.call(ctx, "userManager.getUserInfoAll", nil, nil)
	if err != nil {
		return nil, err
	}
	if !r.ok() {
		return nil, provider.NewError(provider.KindUnsupported, nil, "Das Gerät liefert keine Benutzerliste")
	}
	var p struct {
		Users []struct {
			Name     string `json:"Name"`
			Group    string `json:"Group"`
			Reserved any    `json:"Reserved"`
		} `json:"users"`
	}
	if err := decodeParams(r, &p); err != nil {
		return nil, provider.NewError(provider.KindProtocol, err, "Benutzerliste nicht lesbar")
	}
	var out []provider.DeviceUser
	for _, u := range p.Users {
		if u.Name != "" {
			out = append(out, provider.DeviceUser{Name: u.Name, Group: u.Group, Reserved: jbool(u.Reserved), Self: u.Name == d.cfg.Username})
		}
	}
	return out, nil
}

// canChangePassword: the stored password form is known.
func (d *rpcDriver) canChangePassword() bool {
	enc, realm := d.rpc.scheme()
	return strings.EqualFold(enc, "Default") && realm != ""
}

func (d *rpcDriver) modifyPassword(ctx context.Context, user, oldHash, newHash string) error {
	r, err := d.rpc.call(ctx, "userManager.modifyPassword", map[string]any{"name": user, "pwd": newHash, "pwdOld": oldHash}, nil)
	if err != nil {
		return err
	}
	if !r.ok() {
		msg := ""
		if r.Error != nil && r.Error.Message != "" {
			msg = ": " + r.Error.Message
		}
		return provider.NewError(provider.KindProtocol, nil, "Das Gerät lehnt die Passwortänderung ab%s", msg)
	}
	return nil
}

// ChangePassword sets a user's password. oldPass "" means the login CNM
// uses (its password is known).
func (d *rpcDriver) ChangePassword(ctx context.Context, user, oldPass, newPass string) error {
	enc, realm := d.rpc.scheme()
	if !d.canChangePassword() {
		return provider.NewError(provider.KindUnsupported, nil, "Bei diesem Gerät kann CNM Passwörter nicht ändern (Anmeldeverfahren %q)", enc)
	}
	if oldPass == "" {
		if user != d.cfg.Username {
			return provider.NewError(provider.KindConfig, nil, "Für diesen Benutzer bitte das bisherige Passwort angeben")
		}
		oldPass = d.cfg.Password
	}
	oldHash, newHash := upperMD5(user+":"+realm+":"+oldPass), upperMD5(user+":"+realm+":"+newPass)
	if err := d.modifyPassword(ctx, user, oldHash, newHash); err != nil {
		return err
	}
	test := d.rpc.clone(user, newPass, quietEnv{d.env})
	defer test.close()
	vctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	verr := test.login(vctx)
	cancel()
	if verr == nil {
		test.logout()
		return nil
	}
	return provider.PasswordRestore(verr, d.modifyPassword(ctx, user, newHash, oldHash))
}
