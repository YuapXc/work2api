package app

import wbruntime "work2api/internal/workbuddy/runtime"

var dailyModelLimit = wbruntime.DailyModelLimit
var upstreamErrorText = wbruntime.UpstreamErrorText
var parseModelAliases = wbruntime.ParseModelAliases
var jsonError = wbruntime.JsonError
var errAnthropic = wbruntime.ErrAnthropic
var safeErr = wbruntime.SafeErr
var convUsage = wbruntime.ConvUsage
var toStr = wbruntime.ToStr
var orDash = wbruntime.OrDash
var itoa = wbruntime.Itoa

const cooldownSoft = wbruntime.CooldownSoft
const cooldownHard = wbruntime.CooldownHard
const sessionStickyMaxCooldown = wbruntime.SessionStickyMaxCooldown

var limitResetRe = wbruntime.LimitResetRe
