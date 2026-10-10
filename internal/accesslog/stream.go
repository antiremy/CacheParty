package accesslog

import (
	"strconv"
	"time"
)

// StreamEntry is one logged :443 passthrough connection.
type StreamEntry struct {
	RemoteAddr    string
	Time          time.Time // when the connection was accepted
	Protocol      string    // "TCP"
	Status        int       // nginx stream codes: 200, 400, 403, 502
	BytesSent     int64     // to the client
	BytesReceived int64     // from the client
	SessionTime   time.Duration
	ServerName    string // SNI, empty when the client sent none
}

// AppendStream appends the stream-access.log line (with trailing newline):
//
//	$remote_addr [$time_local] $protocol $status $bytes_sent $bytes_received
//	$session_time "$ssl_preread_server_name"
func AppendStream(dst []byte, e *StreamEntry) []byte {
	dst = appendNginx(dst, e.RemoteAddr)
	dst = append(dst, " ["...)
	dst = e.Time.AppendFormat(dst, timeLocal)
	dst = append(dst, "] "...)
	dst = appendNginx(dst, e.Protocol)
	dst = append(dst, ' ')
	dst = strconv.AppendInt(dst, int64(e.Status), 10)
	dst = append(dst, ' ')
	dst = strconv.AppendInt(dst, e.BytesSent, 10)
	dst = append(dst, ' ')
	dst = strconv.AppendInt(dst, e.BytesReceived, 10)
	dst = append(dst, ' ')
	dst = strconv.AppendFloat(dst, e.SessionTime.Seconds(), 'f', 3, 64)
	dst = append(dst, " \""...)
	dst = appendNginx(dst, e.ServerName)
	return append(dst, "\"\n"...)
}
