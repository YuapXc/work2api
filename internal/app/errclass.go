package app

import wbruntime "work2api/internal/workbuddy/runtime"

var hitFold = wbruntime.HitFold
var hitLower = wbruntime.HitLower
var codeMarker = wbruntime.CodeMarker
var jsonRoot = wbruntime.JsonRoot
var isModelBlocked = wbruntime.IsModelBlocked
var isChannelDenied = wbruntime.IsChannelDenied
var hasBusinessEnvelope = wbruntime.HasBusinessEnvelope
var businessCode = wbruntime.BusinessCode
var isWAFBlocked = wbruntime.IsWAFBlocked
var parseRateReset = wbruntime.ParseRateReset
var parseRetryAfter = wbruntime.ParseRetryAfter
var nextDay4am = wbruntime.NextDay4am
var softRateCooldown = wbruntime.SoftRateCooldown
var wafCooldown = wbruntime.WafCooldown
var clampFloat = wbruntime.ClampFloat
var classifyUpstream = wbruntime.ClassifyUpstream
var actionFor = wbruntime.ActionFor
var maxFloat = wbruntime.MaxFloat

type errAction = wbruntime.ErrAction

const errNone = wbruntime.ErrNone
const errHardCredit = wbruntime.ErrHardCredit
const errSoftRate = wbruntime.ErrSoftRate
const errSessionDead = wbruntime.ErrSessionDead
const errNotFound = wbruntime.ErrNotFound
const errServer = wbruntime.ErrServer
const errChannelDenied = wbruntime.ErrChannelDenied
const errContentBlocked = wbruntime.ErrContentBlocked
const errBadParams = wbruntime.ErrBadParams
const errAccountFault = wbruntime.ErrAccountFault
const errModelBlocked = wbruntime.ErrModelBlocked
const errWAFBlock = wbruntime.ErrWAFBlock
const errPromptTooLong = wbruntime.ErrPromptTooLong
const errImageInvalid = wbruntime.ErrImageInvalid
const errClient = wbruntime.ErrClient
const softCooldownSec = wbruntime.SoftCooldownSec
const notFoundCooldown = wbruntime.NotFoundCooldown
const wafCooldownBase = wbruntime.WafCooldownBase
const serverCooldown = wbruntime.ServerCooldown
const softRateMax = wbruntime.SoftRateMax
const modelBlockBaseTTL = wbruntime.ModelBlockBaseTTL
const retryAfterSanity = wbruntime.RetryAfterSanity

var rateMarkers = wbruntime.RateMarkers
var sessionDeadMarkers = wbruntime.SessionDeadMarkers
var accountFaultMarkers = wbruntime.AccountFaultMarkers
var promptTooLongMarkers = wbruntime.PromptTooLongMarkers
var contentBlockedMarkers = wbruntime.ContentBlockedMarkers
var badParamsMarkers = wbruntime.BadParamsMarkers
var invalidImageMarkers = wbruntime.InvalidImageMarkers
var hardCreditMarkers = wbruntime.HardCreditMarkers
var promptTooLongStatuses = wbruntime.PromptTooLongStatuses
var resetRe = wbruntime.ResetRe
var retryAfterHeaders = wbruntime.RetryAfterHeaders
var codeMarkerCache = wbruntime.CodeMarkerCache

type ErrKind = wbruntime.ErrKind
