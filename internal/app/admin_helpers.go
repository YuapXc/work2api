package app

import wbruntime "work2api/internal/workbuddy/runtime"

var safeUID = wbruntime.SafeUID
var trimDash = wbruntime.TrimDash
var hiddenAccountKey = wbruntime.HiddenAccountKey
var billingCheckin = wbruntime.BillingCheckin

func (o *Orchestrator) registerAuthUpload(data []byte) (map[string]any, *apiError) {
	return o.wb.RegisterAuthUpload(data)
}
func (o *Orchestrator) deleteAccount(uid string) error { return o.wb.DeleteAccount(uid) }
