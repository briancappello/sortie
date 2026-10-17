// Copyright (c) 2026 Probo Inc <hello@probo.com>.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.
//
// Derived from go.probo.inc/probo/pkg/prosemirror (safeLinkHref and
// safeImageSrc in html_renderer.go at commit
// a89724c2d519732aa13caa72331f03d8b3f80976), unmodified.

package markup

import (
	"net/url"
	"strings"
)

func safeLinkHref(href string) string {
	href = strings.TrimSpace(href)
	if href == "" {
		return "#"
	}

	if href[0] == '#' {
		return href
	}

	if strings.HasPrefix(href, "/") {
		if len(href) > 1 && (href[1] == '/' || href[1] == '\\') {
			return "#"
		}

		return href
	}

	u, err := url.Parse(href)
	if err != nil {
		return "#"
	}

	if u.Scheme != "" {
		switch strings.ToLower(u.Scheme) {
		case "http", "https", "mailto", "tel":
			return href
		default:
			return "#"
		}
	}

	if u.Host != "" {
		return "#"
	}

	return href
}

func safeImageSrc(src string) string {
	src = strings.TrimSpace(src)
	if src == "" {
		return ""
	}

	if strings.HasPrefix(src, "/") {
		if len(src) > 1 && (src[1] == '/' || src[1] == '\\') {
			return ""
		}

		return src
	}

	u, err := url.Parse(src)
	if err != nil {
		return ""
	}

	if u.Scheme != "" {
		switch strings.ToLower(u.Scheme) {
		case "http", "https", "data":
			return src
		default:
			return ""
		}
	}

	if u.Host != "" {
		return ""
	}

	return src
}
