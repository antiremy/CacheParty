// Package accesslog writes nginx-compatible access logs.
//
// The cachelog format is byte-for-byte what lancachenet/monolithic emits
// because existing lancache statistics tools parse it.
package accesslog

import (
	"strconv"
	"time"
)

// Entry is one logged request.
type Entry struct {
	CacheID       string
	RemoteAddr    string
	XForwardedFor string
	RemoteUser    string
	Time          time.Time
	Method        string
	URI           string // request URI including query, as sent
	Proto         string
	Status        int
	BodyBytes     int64
	Referer       string
	UserAgent     string
	CacheStatus   string
	Host          string // without port, as nginx's $host
	Range         string
}

const (
	timeLocal   = "02/Jan/2006:15:04:05 -0700"
	hexUpper    = "0123456789ABCDEF"
	hexLowerStr = "0123456789abcdef"
)

// AppendCachelog appends the cachelog line (with trailing newline) to dst:
//
//	[$cacheidentifier] $remote_addr / $http_x_forwarded_for - $remote_user
//	[$time_local] "$request" $status $body_bytes_sent "$http_referer"
//	"$http_user_agent" "$upstream_cache_status" "$host" "$http_range"
func AppendCachelog(dst []byte, e *Entry) []byte {
	dst = append(dst, '[')
	dst = appendNginx(dst, e.CacheID)
	dst = append(dst, "] "...)
	dst = appendNginx(dst, e.RemoteAddr)
	dst = append(dst, " / "...)
	dst = appendNginx(dst, e.XForwardedFor)
	dst = append(dst, " - "...)
	dst = appendNginx(dst, e.RemoteUser)
	dst = append(dst, " ["...)
	dst = e.Time.AppendFormat(dst, timeLocal)
	dst = append(dst, "] \""...)
	dst = appendNginxRequest(dst, e)
	dst = append(dst, "\" "...)
	dst = strconv.AppendInt(dst, int64(e.Status), 10)
	dst = append(dst, ' ')
	dst = strconv.AppendInt(dst, e.BodyBytes, 10)
	dst = append(dst, " \""...)
	dst = appendNginx(dst, e.Referer)
	dst = append(dst, "\" \""...)
	dst = appendNginx(dst, e.UserAgent)
	dst = append(dst, "\" \""...)
	dst = appendNginx(dst, e.CacheStatus)
	dst = append(dst, "\" \""...)
	dst = appendNginx(dst, e.Host)
	dst = append(dst, "\" \""...)
	dst = appendNginx(dst, e.Range)
	return append(dst, "\"\n"...)
}

// AppendJSON appends one cachelog-json line (with trailing newline).
func AppendJSON(dst []byte, e *Entry) []byte {
	dst = append(dst, `{"timestamp":"`...)
	dst = e.Time.AppendFormat(dst, "2006-01-02T15:04:05-07:00")
	dst = jsonField(dst, `","cache_identifier":"`, e.CacheID)
	dst = jsonField(dst, `","remote_addr":"`, e.RemoteAddr)
	dst = jsonField(dst, `","forwarded_for":"`, e.XForwardedFor)
	dst = jsonField(dst, `","remote_user":"`, e.RemoteUser)
	dst = append(dst, `","status":"`...)
	dst = strconv.AppendInt(dst, int64(e.Status), 10)
	dst = append(dst, `","bytes_sent":"`...)
	dst = strconv.AppendInt(dst, e.BodyBytes, 10)
	dst = jsonField(dst, `","referer":"`, e.Referer)
	dst = jsonField(dst, `","user_agent":"`, e.UserAgent)
	dst = jsonField(dst, `","upstream_cache_status":"`, e.CacheStatus)
	dst = jsonField(dst, `","host":"`, e.Host)
	dst = jsonField(dst, `","http_range":"`, e.Range)
	return append(dst, "\"}\n"...)
}

func jsonField(dst []byte, prefix, v string) []byte {
	dst = append(dst, prefix...)
	return appendJSON(dst, v)
}

// appendNginxRequest writes `$request` ("METHOD URI PROTO") escaped.
func appendNginxRequest(dst []byte, e *Entry) []byte {
	dst = appendEscaped(dst, e.Method)
	dst = append(dst, ' ')
	dst = appendEscaped(dst, e.URI)
	dst = append(dst, ' ')
	return appendEscaped(dst, e.Proto)
}

// appendNginx writes v, or "-" when empty, escaped like nginx's default
// log escaping.
func appendNginx(dst []byte, v string) []byte {
	if v == "" {
		return append(dst, '-')
	}
	return appendEscaped(dst, v)
}

// appendEscaped mirrors nginx's default log escaping: '"', '\' and any byte
// outside 0x20..0x7E become \xHH (uppercase hex).
func appendEscaped(dst []byte, v string) []byte {
	for i := 0; i < len(v); i++ {
		c := v[i]
		if c < 0x20 || c > 0x7E || c == '"' || c == '\\' {
			dst = append(dst, '\\', 'x', hexUpper[c>>4], hexUpper[c&0xF])
		} else {
			dst = append(dst, c)
		}
	}
	return dst
}

// appendJSON mirrors nginx's escape=json; empty values become "-".
func appendJSON(dst []byte, v string) []byte {
	if v == "" {
		return append(dst, '-')
	}
	for i := 0; i < len(v); i++ {
		switch c := v[i]; {
		case c == '"' || c == '\\':
			dst = append(dst, '\\', c)
		case c == '\b':
			dst = append(dst, '\\', 'b')
		case c == '\f':
			dst = append(dst, '\\', 'f')
		case c == '\n':
			dst = append(dst, '\\', 'n')
		case c == '\r':
			dst = append(dst, '\\', 'r')
		case c == '\t':
			dst = append(dst, '\\', 't')
		case c < 0x20:
			dst = append(dst, '\\', 'u', '0', '0', hexLowerStr[c>>4], hexLowerStr[c&0xF])
		default:
			dst = append(dst, c)
		}
	}
	return dst
}
