package main

import (
	"net"
	"testing"
)

func TestServerBaseURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		addr net.Addr
		want string
	}{
		{&net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 7678}, "http://127.0.0.1:7678"},
		{&net.TCPAddr{IP: net.ParseIP("0.0.0.0"), Port: 8080}, "http://127.0.0.1:8080"},
		{&net.TCPAddr{IP: net.ParseIP("::"), Port: 8080}, "http://[::1]:8080"},
		{&net.TCPAddr{IP: net.ParseIP("::1"), Port: 9}, "http://[::1]:9"},
		{&net.TCPAddr{IP: net.ParseIP("192.168.1.5"), Port: 80}, "http://192.168.1.5:80"},
	}
	for _, tt := range tests {
		if got := serverBaseURL(tt.addr); got != tt.want {
			t.Errorf("serverBaseURL(%v) = %q, want %q", tt.addr, got, tt.want)
		}
	}
}
