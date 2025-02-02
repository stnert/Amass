// Copyright © by Jeff Foley 2017-2023. All rights reserved.
// Use of this source code is governed by Apache 2 LICENSE that can be found in the LICENSE file.
// SPDX-License-Identifier: Apache-2.0

package scripting

import (
	"context"
	"net"
	"time"

	amassnet "github.com/OWASP/Amass/v3/net"
	"github.com/OWASP/Amass/v3/net/http"
	"github.com/OWASP/Amass/v3/requests"
	bf "github.com/tylertreat/BoomFilters"
	lua "github.com/yuin/gopher-lua"
)

func (s *Script) genNewName(ctx context.Context, name string) {
	s.newNameWithSrc(ctx, name, s.Description(), s.String())
}

func (s *Script) newNameWithSrc(ctx context.Context, name, tag, src string) {
	if domain := s.sys.Config().WhichDomain(name); domain != "" {
		select {
		case <-ctx.Done():
		case <-s.Done():
		default:
			s.Output() <- &requests.DNSRequest{
				Name:   name,
				Domain: domain,
				Tag:    tag,
				Source: src,
			}
		}
	}
}

// Wrapper so that scripts can send a discovered FQDN to Amass.
func (s *Script) newName(L *lua.LState) int {
	if ctx, err := extractContext(L.CheckUserData(1)); err == nil && !contextExpired(ctx) {
		if n := L.CheckString(2); n != "" {
			if name := s.subre.FindString(n); name != "" {
				s.genNewName(ctx, name)
			}
		}
	}
	return 0
}

// Wrapper so that scripts can send FQDNs found in the content to Amass.
func (s *Script) sendNames(L *lua.LState) int {
	var num int

	if ctx, err := extractContext(L.CheckUserData(1)); err == nil && !contextExpired(ctx) {
		if content := L.CheckString(2); content != "" {
			num = s.internalSendNames(ctx, content)
		}
	}

	L.Push(lua.LNumber(num))
	return 1
}

func (s *Script) internalSendNames(ctx context.Context, content string) int {
	return s.internalSendNamesWithSrc(ctx, content, s.Description(), s.String())
}

func (s *Script) internalSendNamesWithSrc(ctx context.Context, content, tag, src string) int {
	filter := bf.NewDefaultStableBloomFilter(1000, 0.01)
	defer filter.Reset()

	var count int
	for _, name := range s.subre.FindAllString(string(content), -1) {
		if n := http.CleanName(name); n != "" && !filter.TestAndAdd([]byte(n)) {
			s.newNameWithSrc(ctx, n, tag, src)
			count++
		}
	}
	return count
}

func (s *Script) sendDNSRecords(L *lua.LState) int {
	ctx, err := extractContext(L.CheckUserData(1))
	if err != nil || contextExpired(ctx) {
		return 0
	}

	name := L.CheckString(2)
	if name == "" {
		return 0
	}

	array := L.CheckTable(3)
	if array == nil {
		return 0
	}

	var records []requests.DNSAnswer
	array.ForEach(func(k, v lua.LValue) {
		if tbl, ok := v.(*lua.LTable); ok {
			var qtype int
			if lv := L.GetField(tbl, "rrtype"); lv != nil {
				if n, ok := lv.(lua.LNumber); ok {
					qtype = int(n)
				}
			}

			name, _ := getStringField(L, tbl, "rrname")
			data, _ := getStringField(L, tbl, "rrdata")
			if name != "" && qtype != 0 && data != "" {
				records = append(records, requests.DNSAnswer{
					Name: name,
					Type: qtype,
					Data: data,
				})
			}
		}
	})

	s.internalSendDNSRecords(ctx, name, records)
	return 0
}

func (s *Script) internalSendDNSRecords(ctx context.Context, name string, records []requests.DNSAnswer) {
	if domain := s.sys.Config().WhichDomain(name); domain != "" {
		select {
		case <-ctx.Done():
		case <-s.Done():
		default:
			s.Output() <- &requests.DNSRequest{
				Name:    name,
				Domain:  domain,
				Records: records,
				Tag:     s.Description(),
				Source:  s.String(),
			}
		}
	}
}

// Wrapper so that scripts can send discovered IP addresses to Amass.
func (s *Script) newAddr(L *lua.LState) int {
	ip := net.ParseIP(L.CheckString(2))

	if ip == nil {
		return 0
	}
	if reserved, _ := amassnet.IsReservedAddress(ip.String()); reserved {
		return 0
	}
	if ctx, err := extractContext(L.CheckUserData(1)); err == nil && !contextExpired(ctx) {
		if name := L.CheckString(3); err == nil && name != "" {
			if domain := s.sys.Config().WhichDomain(name); domain != "" {
				select {
				case <-ctx.Done():
				case <-s.Done():
				default:
					s.Output() <- &requests.AddrRequest{
						Address: ip.String(),
						Domain:  domain,
						Tag:     s.SourceType,
						Source:  s.String(),
					}
				}
			}
		}
	}
	return 0
}

// Wrapper so that scripts can send discovered ASNs to Amass.
func (s *Script) newASN(L *lua.LState) int {
	if ctx, err := extractContext(L.CheckUserData(1)); err == nil && !contextExpired(ctx) {
		if params := L.CheckTable(2); err == nil && params != nil {
			addr, _ := getStringField(L, params, "addr")
			ip := net.ParseIP(addr)
			if ip == nil {
				return 0
			}

			addr = ip.String()
			if reserved, _ := amassnet.IsReservedAddress(addr); reserved {
				return 0
			}

			asn, _ := getNumberField(L, params, "asn")
			prefix, _ := getStringField(L, params, "prefix")
			desc, _ := getStringField(L, params, "desc")
			if asn == 0 || prefix == "" || desc == "" {
				return 0
			}

			_, cidr, err := net.ParseCIDR(prefix)
			if err != nil {
				return 0
			}

			netblocks := []string{cidr.String()}
			lv := L.GetField(params, "netblocks")
			if tbl, ok := lv.(*lua.LTable); ok {
				tbl.ForEach(func(_, v lua.LValue) {
					if _, cidr, err := net.ParseCIDR(v.String()); err == nil {
						netblocks = append(netblocks, cidr.String())
					}
				})
			}

			cc, _ := getStringField(L, params, "cc")
			registry, _ := getStringField(L, params, "registry")
			s.sys.Cache().Update(&requests.ASNRequest{
				Address:        addr,
				ASN:            int(asn),
				Prefix:         prefix,
				CC:             cc,
				Registry:       registry,
				AllocationDate: time.Now(),
				Description:    desc,
				Netblocks:      netblocks,
				Tag:            s.SourceType,
				Source:         s.String(),
			})
		}
	}
	return 0
}

// Wrapper so that scripts can send discovered associated domains to Amass.
func (s *Script) associated(L *lua.LState) int {
	if ctx, err := extractContext(L.CheckUserData(1)); err == nil && !contextExpired(ctx) {
		if domain, assoc := L.CheckString(2), L.CheckString(3); err == nil && domain != "" && assoc != "" && domain != assoc {
			select {
			case <-ctx.Done():
			case <-s.Done():
			default:
				s.Output() <- &requests.WhoisRequest{
					Domain:     domain,
					NewDomains: []string{assoc},
					Tag:        s.SourceType,
					Source:     s.String(),
				}
			}
		}
	}
	return 0
}
