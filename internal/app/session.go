package app

import wbruntime "work2api/internal/workbuddy/runtime"

var principalSessionKey = wbruntime.PrincipalSessionKey
var scopedSessionKey = wbruntime.ScopedSessionKey
var withRequestSessionIdentity = wbruntime.WithRequestSessionIdentity
var requestSessionKey = wbruntime.RequestSessionKey
var boundedSessionID = wbruntime.BoundedSessionID
var extractSessionKey = wbruntime.ExtractSessionKey
var newSessionRouter = wbruntime.NewSessionRouter

type requestSessionIdentity = wbruntime.RequestSessionIdentity
type requestSessionIdentityKey = wbruntime.RequestSessionIdentityKey
type sessionEntry = wbruntime.SessionEntry
type sessionRouter = wbruntime.SessionRouter
