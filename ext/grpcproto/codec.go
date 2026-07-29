package grpcproto

import (
	"encoding/json"
	"net/http"

	ext "github.com/AlistGo/alist-ext"
)

// This file holds the pure ext.* <-> proto conversions shared by the host proxy
// (internal/extension/grpc/host) and the plugin serve helper (ext/grpcplugin).
// It has no grpc / go-plugin dependency so both sides link the same codec.

// --- errors ---

// ErrorToProto encodes an error as an ErrorProto. A nil error yields nil. An
// *ext.Error keeps its code; any other error is carried with an empty code and
// its message.
func ErrorToProto(err error) *ErrorProto {
	if err == nil {
		return nil
	}
	if code, ok := ext.CodeOf(err); ok {
		return &ErrorProto{Code: string(code), Message: err.Error()}
	}
	return &ErrorProto{Message: err.Error()}
}

// ErrorFromProto reconstructs an error from an ErrorProto. A nil proto or empty
// code+message yields nil. A non-empty code becomes an *ext.Error (so the host
// boundary can map it back to a sentinel via ext.CodeOf); a code-less error
// becomes a plain error carrying the message.
func ErrorFromProto(e *ErrorProto) error {
	if e == nil || (e.Code == "" && e.Message == "") {
		return nil
	}
	if e.Code != "" {
		return ext.NewError(ext.ErrCode(e.Code), e.Message)
	}
	return &plainError{msg: e.Message}
}

type plainError struct{ msg string }

func (e *plainError) Error() string { return e.msg }

// --- ObjRef ---

func RefToProto(r ext.ObjRef) *ObjRef {
	return &ObjRef{StorageId: r.StorageID, Anchor: r.Anchor}
}

func RefFromProto(r *ObjRef) ext.ObjRef {
	if r == nil {
		return ext.ObjRef{}
	}
	return ext.ObjRef{StorageID: r.StorageId, Anchor: r.Anchor}
}

// --- Obj ---

func ObjToProto(o ext.Obj) *Obj {
	return &Obj{
		Name:     o.Name,
		Size:     o.Size,
		Modified: o.Modified,
		Created:  o.Created,
		IsDir:    o.IsDir,
		Id:       o.ID,
		Path:     o.Path,
		Hashes:   o.Hashes,
		Thumb:    o.Thumb,
		Url:      o.URL,
	}
}

func ObjFromProto(o *Obj) ext.Obj {
	if o == nil {
		return ext.Obj{}
	}
	return ext.Obj{
		Name:     o.Name,
		Size:     o.Size,
		Modified: o.Modified,
		Created:  o.Created,
		IsDir:    o.IsDir,
		ID:       o.Id,
		Path:     o.Path,
		Hashes:   o.Hashes,
		Thumb:    o.Thumb,
		URL:      o.Url,
	}
}

func ObjsToProto(objs []ext.Obj) []*Obj {
	out := make([]*Obj, 0, len(objs))
	for _, o := range objs {
		out = append(out, ObjToProto(o))
	}
	return out
}

func ObjsFromProto(objs []*Obj) []ext.Obj {
	out := make([]ext.Obj, 0, len(objs))
	for _, o := range objs {
		out = append(out, ObjFromProto(o))
	}
	return out
}

// ObjPtrToProto encodes an optional *ext.Obj (nil-safe).
func ObjPtrToProto(o *ext.Obj) *Obj {
	if o == nil {
		return nil
	}
	return ObjToProto(*o)
}

// ObjPtrFromProto decodes an optional *ext.Obj (nil-safe).
func ObjPtrFromProto(o *Obj) *ext.Obj {
	if o == nil {
		return nil
	}
	v := ObjFromProto(o)
	return &v
}

// --- http.Header ---

func HeaderToProto(h http.Header) map[string]*HeaderValue {
	if h == nil {
		return nil
	}
	out := make(map[string]*HeaderValue, len(h))
	for k, v := range h {
		out[k] = &HeaderValue{Values: v}
	}
	return out
}

func HeaderFromProto(m map[string]*HeaderValue) http.Header {
	if len(m) == 0 {
		return nil
	}
	out := make(http.Header, len(m))
	for k, v := range m {
		if v != nil {
			out[k] = v.Values
		}
	}
	return out
}

// --- Link ---

func LinkToProto(l *ext.Link) *Link {
	if l == nil {
		return nil
	}
	return &Link{
		ByteSource:   string(l.ByteSource),
		Url:          l.URL,
		Header:       HeaderToProto(l.Header),
		StreamId:     l.StreamID,
		RangeSupport: l.RangeSupport,
		Expiration:   l.Expiration,
		IpCacheKey:   l.IPCacheKey,
		Concurrency:  int32(l.Concurrency),
		PartSize:     int32(l.PartSize),
	}
}

func LinkFromProto(l *Link) *ext.Link {
	if l == nil {
		return nil
	}
	return &ext.Link{
		ByteSource:   ext.ByteSource(l.ByteSource),
		URL:          l.Url,
		Header:       HeaderFromProto(l.Header),
		StreamID:     l.StreamId,
		RangeSupport: l.RangeSupport,
		Expiration:   l.Expiration,
		IPCacheKey:   l.IpCacheKey,
		Concurrency:  int(l.Concurrency),
		PartSize:     int(l.PartSize),
	}
}

// --- args ---

func ListArgsToProto(a ext.ListArgs) *ListArgs {
	return &ListArgs{ReqPath: a.ReqPath, Refresh: a.Refresh}
}

func ListArgsFromProto(a *ListArgs) ext.ListArgs {
	if a == nil {
		return ext.ListArgs{}
	}
	return ext.ListArgs{ReqPath: a.ReqPath, Refresh: a.Refresh}
}

func LinkArgsToProto(a ext.LinkArgs) *LinkArgs {
	return &LinkArgs{
		Ip:       a.IP,
		Header:   HeaderToProto(a.Header),
		Type:     a.Type,
		Redirect: a.Redirect,
	}
}

func LinkArgsFromProto(a *LinkArgs) ext.LinkArgs {
	if a == nil {
		return ext.LinkArgs{}
	}
	return ext.LinkArgs{
		IP:       a.Ip,
		Header:   HeaderFromProto(a.Header),
		Type:     a.Type,
		Redirect: a.Redirect,
	}
}

// --- Range ---

func RangeToProto(r ext.Range) *Range {
	return &Range{Start: r.Start, Length: r.Length}
}

func RangeFromProto(r *Range) ext.Range {
	if r == nil {
		return ext.Range{}
	}
	return ext.Range{Start: r.Start, Length: r.Length}
}

// --- UploadInfo ---

func UploadInfoToProto(i ext.UploadInfo) *UploadInfo {
	return &UploadInfo{
		Name:     i.Name,
		Size:     i.Size,
		Mimetype: i.Mimetype,
		Hashes:   i.Hashes,
		Exist:    ObjPtrToProto(i.Exist),
	}
}

func UploadInfoFromProto(i *UploadInfo) ext.UploadInfo {
	if i == nil {
		return ext.UploadInfo{}
	}
	return ext.UploadInfo{
		Name:     i.Name,
		Size:     i.Size,
		Mimetype: i.Mimetype,
		Hashes:   i.Hashes,
		Exist:    ObjPtrFromProto(i.Exist),
	}
}

// --- Token (WS-8 AuthProvider) ---

// TokenToProto encodes an ext.Token. Raw (map[string]any) is carried as
// json.Marshal'd bytes (raw_json) — same "it's already a JSON contract"
// rationale as ManifestReply.manifest_json.
func TokenToProto(t ext.Token) *Token {
	var raw []byte
	if len(t.Raw) > 0 {
		raw, _ = json.Marshal(t.Raw) // best-effort: a marshal failure yields empty raw_json, not a lost token
	}
	return &Token{
		AccessToken:  t.AccessToken,
		RefreshToken: t.RefreshToken,
		ExpiresIn:    t.ExpiresIn,
		TokenType:    t.TokenType,
		RawJson:      raw,
	}
}

func TokenFromProto(t *Token) ext.Token {
	if t == nil {
		return ext.Token{}
	}
	out := ext.Token{
		AccessToken:  t.AccessToken,
		RefreshToken: t.RefreshToken,
		ExpiresIn:    t.ExpiresIn,
		TokenType:    t.TokenType,
	}
	if len(t.RawJson) > 0 {
		var raw map[string]any
		if err := json.Unmarshal(t.RawJson, &raw); err == nil {
			out.Raw = raw
		}
	}
	return out
}

// --- AuthNMethod (design §2.2) ---

// MethodInfoToProto/MethodInfoFromProto carry ext.AuthMethodInfo. AuthKind is a
// uint8 in Go, widened to the proto's uint32 (proto3 has no uint8) and narrowed
// back; the enum numbering is pinned by the SDK.
func MethodInfoToProto(i ext.AuthMethodInfo) *MethodInfoReply {
	return &MethodInfoReply{Id: i.ID, Kind: uint32(i.Kind), Display: i.Display}
}

func MethodInfoFromProto(i *MethodInfoReply) ext.AuthMethodInfo {
	if i == nil {
		return ext.AuthMethodInfo{}
	}
	return ext.AuthMethodInfo{ID: i.Id, Kind: ext.AuthKind(i.Kind), Display: i.Display}
}

func AuthNRequestToProto(principal ext.Principal, req ext.AuthNRequest) *BeginRequest {
	return &BeginRequest{
		Principal:   string(principal),
		RedirectUri: req.RedirectURI,
		State:       req.State,
		Params:      req.Params,
	}
}

func AuthNRequestFromProto(r *BeginRequest) (ext.Principal, ext.AuthNRequest) {
	if r == nil {
		return "", ext.AuthNRequest{}
	}
	return ext.Principal(r.Principal), ext.AuthNRequest{
		RedirectURI: r.RedirectUri,
		State:       r.State,
		Params:      r.Params,
	}
}

func AuthNChallengeToProto(c ext.AuthNChallenge) *BeginReply {
	return &BeginReply{
		ChallengeId: c.ChallengeID,
		RedirectUrl: c.RedirectURL,
		Data:        c.Data,
	}
}

func AuthNChallengeFromProto(r *BeginReply) ext.AuthNChallenge {
	if r == nil {
		return ext.AuthNChallenge{}
	}
	return ext.AuthNChallenge{
		ChallengeID: r.ChallengeId,
		RedirectURL: r.RedirectUrl,
		Data:        r.Data,
	}
}

func AuthNResponseToProto(req ext.AuthNResponse) *FinishRequest {
	return &FinishRequest{
		ChallengeId: req.ChallengeID,
		Params:      req.Params,
		Data:        req.Data,
	}
}

func AuthNResponseFromProto(r *FinishRequest) ext.AuthNResponse {
	if r == nil {
		return ext.AuthNResponse{}
	}
	return ext.AuthNResponse{
		ChallengeID: r.ChallengeId,
		Params:      r.Params,
		Data:        r.Data,
	}
}

func AuthNResultToProto(res ext.AuthNResult) *AuthNResultProto {
	return &AuthNResultProto{
		Username:      res.Username,
		Verified:      res.Verified,
		AutoProvision: provisionHintToProto(res.AutoProvision),
	}
}

func AuthNResultFromProto(r *AuthNResultProto) ext.AuthNResult {
	if r == nil {
		return ext.AuthNResult{}
	}
	return ext.AuthNResult{
		Username:      r.Username,
		Verified:      r.Verified,
		AutoProvision: provisionHintFromProto(r.AutoProvision),
	}
}

func provisionHintToProto(h *ext.ProvisionHint) *ProvisionHintProto {
	if h == nil {
		return nil
	}
	return &ProvisionHintProto{
		DisplayName: h.DisplayName,
		Email:       h.Email,
		Attributes:  h.Attributes,
	}
}

func provisionHintFromProto(h *ProvisionHintProto) *ext.ProvisionHint {
	if h == nil {
		return nil
	}
	return &ext.ProvisionHint{
		DisplayName: h.DisplayName,
		Email:       h.Email,
		Attributes:  h.Attributes,
	}
}
