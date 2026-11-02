package trae

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
	"work2api/internal/statebackup"
)

// One marker per existing account; no tokens, user content or unbounded history.
// Mark before sending: after a crash or an ambiguous transport result, read
// status again rather than risk a second accepted claim on the same day.
func (r *Runtime) checkinMarker(id string) string {
	return filepath.Join(r.dir, ".checkin-"+id+".json")
}
func (r *Runtime) checkinSent(id, day string) (bool, error) {
	raw, err := os.ReadFile(r.checkinMarker(id))
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, errors.New("签到领取记录读取失败")
	}
	var stamp struct {
		Day string `json:"day"`
	}
	if len(raw) > 256 || json.Unmarshal(raw, &stamp) != nil {
		return false, errors.New("签到领取记录无效")
	}
	if _, err := time.Parse("2006-01-02", stamp.Day); err != nil {
		return false, errors.New("签到领取记录日期无效")
	}
	return stamp.Day == day, nil
}
func (r *Runtime) markCheckinSent(id, day string) error {
	r.writeMu.Lock()
	defer r.writeMu.Unlock()
	leave := statebackup.Enter()
	defer leave()
	raw, _ := json.Marshal(map[string]string{"day": day})
	f, err := os.CreateTemp(r.dir, ".checkin-write-*")
	if err != nil {
		return errors.New("签到领取记录保存失败")
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(raw)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		return errors.New("签到领取记录保存失败")
	}
	if os.Rename(f.Name(), r.checkinMarker(id)) != nil {
		return errors.New("签到领取记录保存失败")
	}
	return nil
}
func (r *Runtime) clearCheckinSent(id string) error {
	r.writeMu.Lock()
	defer r.writeMu.Unlock()
	leave := statebackup.Enter()
	defer leave()
	err := os.Remove(r.checkinMarker(id))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
