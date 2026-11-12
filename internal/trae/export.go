package trae

// ExportCredentials is only exposed through the authenticated management
// export endpoint. Ownership lives in the database/full backup, never inferred
// from imported labels or credential JSON.
func (r *Runtime) ExportCredentials() (map[string]any, error) {
	r.writeMu.Lock()
	defer r.writeMu.Unlock()
	r.mu.Lock()
	defer r.mu.Unlock()
	accounts := []map[string]any{}
	for id, state := range r.accounts {
		for _, product := range []string{"cn", "solo"} {
			a := state.Identities[product]
			if a == nil {
				continue
			}
			a.RLock()
			doc := map[string]any{"accessToken": a.AccessToken, "refreshToken": a.RefreshToken, "expiresAt": a.ExpiresAt, "domain": a.Domain, "variant": a.Variant, "machineId": a.MachineID, "deviceId": a.DeviceID, "devicePublicKey": a.DevicePublicKey, "devicePrivateKey": a.DevicePrivateKey, "uid": a.UID, "enterpriseId": a.EnterpriseID, "enterpriseConsole": a.EnterpriseConsole, "nickname": a.Nickname}
			a.RUnlock()
			accounts = append(accounts, map[string]any{"id": id, "product": product, "credential": doc})
		}
	}
	return map[string]any{"accounts": accounts, "note": "单账号迁移可导入 credential；完整恢复用户归属、权限及设置请使用完整备份。"}, nil
}
