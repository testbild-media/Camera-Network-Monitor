package sofia

import (
	"context"

	"cnm/internal/provider"
)

const (
	MsgDiskManager    = 1460 // OPStorageManager
	MsgDiskManagerRsp = 1461
)

// FormatDisk formats one partition (OPStorageManager "Clear"): all
// recordings on it are deleted. The device answers when it is done, which
// can take a few minutes; some recorders restart afterwards.
func (d *driver) FormatDisk(ctx context.Context, disk, part int) error {
	ctl, err := d.control()
	if err != nil {
		return err
	}
	r, err := ctl.Request(ctx, MsgDiskManager, MsgDiskManagerRsp, map[string]any{
		"Name":      "OPStorageManager",
		"SessionID": SessionString(ctl.Session()),
		"OPStorageManager": map[string]any{
			"Action":   "Clear",
			"SerialNo": disk,
			"PartNo":   part,
			"Type":     "Data",
		},
	})
	if err != nil {
		if k := provider.KindOf(err); k == provider.KindNetwork || ctx.Err() != nil {
			return provider.NewError(provider.KindNetwork, err, "Die Verbindung wurde während des Formatierens unterbrochen. Das Gerät formatiert vermutlich weiter oder startet neu – in ein paar Minuten die Geräteeinstellungen neu öffnen.")
		}
		return err
	}
	switch ret := r.Ret(); {
	case r.OK(), ret == 150, ret == 602, ret == 603:
		return nil
	case retText[ret] != "":
		return provider.NewError(provider.KindProtocol, nil, "Das Gerät hat das Formatieren abgelehnt: %s (Ret %d)", retText[ret], ret)
	default:
		return provider.NewError(provider.KindProtocol, nil, "Das Gerät hat das Formatieren abgelehnt (Ret %d)", ret)
	}
}
