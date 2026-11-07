package app

import (
	"work2api/internal/requestdiag"
	wbruntime "work2api/internal/workbuddy/runtime"
)

var diagnosticErrorKind = wbruntime.DiagnosticErrorKind
var outputLimits = requestdiag.OutputLimits
var diagnostic = requestdiag.Diagnostic
var usageDiagnostics = requestdiag.UsageDiagnostics

type requestDiagnosticsKey = requestdiag.RequestDiagnosticsKey
type requestDiagnostics = requestdiag.RequestDiagnostics
