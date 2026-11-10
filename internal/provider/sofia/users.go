package sofia

import (
	"context"
	"time"

	"cnm/internal/provider"
)

// The recorder's users and their passwords. A password change is checked
// with a second, separate login before CNM relies on it; when that login
// fails, the old password is restored over the still open session.

const (
	MsgUsers             = 1472
	MsgUsersRsp          = 1473
	MsgModifyPassword    = 1488
	MsgModifyPasswordRsp = 1489
)

// Users lists the recorder's user accounts (no passwords).
func (d *driver) Users(ctx context.Context) ([]provider.DeviceUser, error) {
	ctl, err := d.control()
	if err != nil {
		return nil, err
	}
	r, err := ctl.Request(ctx, MsgUsers, MsgUsersRsp, map[string]any{"Name": "Users", "SessionID": SessionString(ctl.Session())})
	if err != nil {
		return nil, err
	}
	if !r.OK() {
		return nil, provider.NewError(provider.KindUnsupported, nil, "Das Gerät liefert keine Benutzerliste (Ret %d)", r.Ret())
	}
	var out []provider.DeviceUser
	for _, u := range cfgArr(r["Users"]) {
		m := cfgMap(u)
		if name := str(m["Name"]); name != "" {
			out = append(out, provider.DeviceUser{Name: name, Group: str(m["Group"]), Reserved: truthy(m["Reserved"]), Self: name == d.cfg.Username})
		}
	}
	return out, nil
}

func (d *driver) modifyPassword(ctx context.Context, ctl *Control, user, oldHash, newHash string) error {
	r, err := ctl.Request(ctx, MsgModifyPassword, MsgModifyPasswordRsp, map[string]any{
		"Name":        "ModifyPassword",
		"EncryptType": "MD5",
		"UserName":    user,
		"PassWord":    oldHash,
		"NewPassWord": newHash,
		"SessionID":   SessionString(ctl.Session()),
	})
	if err != nil {
		return err
	}
	if !r.OK() {
		if msg, ok := IsAuthRet(r.Ret()); ok {
			return provider.NewError(provider.KindConfig, nil, "Das Gerät lehnt die Passwortänderung ab: %s (Ret %d)", msg, r.Ret())
		}
		return provider.NewError(provider.KindProtocol, nil, "Das Gerät lehnt die Passwortänderung ab (Ret %d)", r.Ret())
	}
	return nil
}

// ChangePassword sets a user's password. oldPass "" means the login CNM
// uses (its password is known).
func (d *driver) ChangePassword(ctx context.Context, user, oldPass, newPass string) error {
	if oldPass == "" {
		if user != d.cfg.Username {
			return provider.NewError(provider.KindConfig, nil, "Für diesen Benutzer bitte das bisherige Passwort angeben")
		}
		oldPass = d.cfg.Password
	}
	ctl, err := d.control()
	if err != nil {
		return err
	}
	oldHash, newHash := SofiaHash(oldPass), SofiaHash(newPass)
	if err := d.modifyPassword(ctx, ctl, user, oldHash, newHash); err != nil {
		return err
	}
	vctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	test, verr := DialControl(vctx, d.cfg.Host, d.port(), user, newPass)
	cancel()
	if verr == nil {
		test.Close()
		return nil
	}
	return provider.PasswordRestore(verr, d.modifyPassword(ctx, ctl, user, newHash, oldHash))
}
