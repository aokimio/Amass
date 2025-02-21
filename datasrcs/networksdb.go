// Copyright © by Jeff Foley 2017-2022. All rights reserved.
// Use of this source code is governed by Apache 2 LICENSE that can be found in the LICENSE file.
// SPDX-License-Identifier: Apache-2.0

package datasrcs

import (
	"context"
	"encoding/json"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/aokimio/Amass/v3/config"
	amassnet "github.com/aokimio/Amass/v3/net"
	"github.com/aokimio/Amass/v3/net/dns"
	"github.com/aokimio/Amass/v3/net/http"
	"github.com/aokimio/Amass/v3/requests"
	"github.com/aokimio/Amass/v3/systems"
	"github.com/caffix/service"
	"github.com/caffix/stringset"
)

const (
	networksdbBaseURL = "https://networksdb.io"
	networksdbAPIPATH = "/api/v1"
)

var (
	networksdbASNLinkRE    = regexp.MustCompile(`Announcing ASN:<\/b> <a class="link_sm" href="(.*)"`)
	networksdbIPLinkRE     = regexp.MustCompile(`<a class="link_sm" href="(\/ip\/[.:a-zA-Z0-9]+)">`)
	networksdbASNRE        = regexp.MustCompile(`AS Number:<\/b> ([0-9]*)<br>`)
	networksdbCIDRRE       = regexp.MustCompile(`CIDR:<\/b>(.*)<br>`)
	networksdbIPPageCIDRRE = regexp.MustCompile(`<b>Network:.* href=".*".*href=".*">(.*)<\/a>`)
	networksdbASNameRE     = regexp.MustCompile(`AS Name:<\/b>(.*)<br>`)
	networksdbCCRE         = regexp.MustCompile(`Location:<\/b>.*href="/country/(.*)">`)
	networksdbDomainsRE    = regexp.MustCompile(`Domains in network`)
	networksdbTableRE      = regexp.MustCompile(`<table class`)
)

// NetworksDB is the Service that handles access to the NetworksDB.io data source.
type NetworksDB struct {
	service.BaseService

	SourceType string
	sys        systems.System
	creds      *config.Credentials
	hasAPIKey  bool
}

// NewNetworksDB returns he object initialized, but not yet started.
func NewNetworksDB(sys systems.System) *NetworksDB {
	n := &NetworksDB{
		SourceType: requests.API,
		sys:        sys,
		hasAPIKey:  true,
	}

	go n.requests()
	n.BaseService = *service.NewBaseService(n, "NetworksDB")
	return n
}

// Description implements the Service interface.
func (n *NetworksDB) Description() string {
	return n.SourceType
}

// OnStart implements the Service interface.
func (n *NetworksDB) OnStart() error {
	n.creds = n.sys.Config().GetDataSourceConfig(n.String()).GetCredentials()

	if n.creds == nil || n.creds.Key == "" {
		n.sys.Config().Log.Printf("%s: API key data was not provided", n.String())
		n.SourceType = requests.SCRAPE
		n.hasAPIKey = false
	}

	n.SetRateLimit(1)
	return nil
}

func (n *NetworksDB) requests() {
	for {
		select {
		case <-n.Done():
			return
		case in := <-n.Input():
			switch req := in.(type) {
			case *requests.ASNRequest:
				n.CheckRateLimit()
				n.asnRequest(context.TODO(), req)
			case *requests.WhoisRequest:
				n.CheckRateLimit()
				n.whoisRequest(context.TODO(), req)
			}
		}
	}
}

func (n *NetworksDB) asnRequest(ctx context.Context, req *requests.ASNRequest) {
	if req.Address == "" && req.ASN == 0 {
		return
	}

	numRateLimitChecks(n, 2)
	if n.hasAPIKey {
		if req.Address != "" {
			n.executeAPIASNAddrQuery(ctx, req.Address)
		} else {
			n.executeAPIASNQuery(ctx, req.ASN, "", nil)
		}
		return
	}

	if req.Address != "" {
		n.executeASNAddrQuery(ctx, req.Address)
	} else {
		set := stringset.New()
		defer set.Close()

		n.executeASNQuery(ctx, req.ASN, "", set)
	}
}

func (n *NetworksDB) executeASNAddrQuery(ctx context.Context, addr string) {
	u := n.getIPURL(addr)
	page, err := http.RequestWebPage(ctx, u, nil, nil, nil)
	if err != nil {
		n.sys.Config().Log.Printf("%s: %s: %v", n.String(), u, err)
		return
	}

	matches := networksdbASNLinkRE.FindStringSubmatch(page)
	if matches == nil || len(matches) < 2 {
		n.sys.Config().Log.Printf("%s: %s: Failed to extract the autonomous system href", n.String(), u)
		return
	}

	numRateLimitChecks(n, 3)
	u = networksdbBaseURL + matches[1]
	page, err = http.RequestWebPage(ctx, u, nil, nil, nil)
	if err != nil {
		n.sys.Config().Log.Printf("%s: %s: %v", n.String(), u, err)
		return
	}

	netblocks := stringset.New()
	defer netblocks.Close()

	for _, match := range networksdbCIDRRE.FindAllStringSubmatch(page, -1) {
		if len(match) >= 2 {
			netblocks.Insert(strings.TrimSpace(match[1]))
		}
	}

	matches = networksdbASNRE.FindStringSubmatch(page)
	if matches == nil || len(matches) < 2 {
		n.sys.Config().Log.Printf("%s: %s: The regular expression failed to extract the ASN", n.String(), u)
		return
	}

	asn, err := strconv.Atoi(strings.TrimSpace(matches[1]))
	if err != nil {
		n.sys.Config().Log.Printf("%s: %s: Failed to extract a valid ASN", n.String(), u)
		return
	}

	n.executeASNQuery(ctx, asn, addr, netblocks)
}

func (n *NetworksDB) getIPURL(addr string) string {
	return networksdbBaseURL + "/ip/" + addr
}

func (n *NetworksDB) executeASNQuery(ctx context.Context, asn int, addr string, netblocks *stringset.Set) {
	numRateLimitChecks(n, 3)
	u := n.getASNURL(asn)
	page, err := http.RequestWebPage(ctx, u, nil, nil, nil)
	if err != nil {
		n.sys.Config().Log.Printf("%s: %s: %v", n.String(), u, err)
		return
	}

	matches := networksdbASNameRE.FindStringSubmatch(page)
	if matches == nil || len(matches) < 2 {
		n.sys.Config().Log.Printf("%s: The regular expression failed to extract the AS name", n.String())
		return
	}
	name := strings.TrimSpace(matches[1])

	matches = networksdbCCRE.FindStringSubmatch(page)
	if matches == nil || len(matches) < 2 {
		n.sys.Config().Log.Printf("%s: The regular expression failed to extract the country code", n.String())
		return
	}
	cc := strings.TrimSpace(matches[1])

	for _, match := range networksdbCIDRRE.FindAllStringSubmatch(page, -1) {
		if len(match) >= 2 {
			netblocks.Insert(strings.TrimSpace(match[1]))
		}
	}

	var prefix string
	if addr != "" {
		ip := net.ParseIP(addr)

		for _, cidr := range netblocks.Slice() {
			if _, ipnet, err := net.ParseCIDR(cidr); err == nil && ipnet.Contains(ip) {
				prefix = cidr
				break
			}
		}
	}
	if prefix == "" && netblocks.Len() > 0 {
		prefix = netblocks.Slice()[0] // TODO order may matter here :shrug:
	}

	n.sys.Cache().Update(&requests.ASNRequest{
		Address:     addr,
		ASN:         asn,
		Prefix:      prefix,
		CC:          cc,
		Description: name + ", " + cc,
		Netblocks:   netblocks.Slice(),
		Tag:         n.SourceType,
		Source:      n.String(),
	})
}

func (n *NetworksDB) getASNURL(asn int) string {
	return networksdbBaseURL + "/autonomous-system/AS" + strconv.Itoa(asn)
}

func (n *NetworksDB) executeAPIASNAddrQuery(ctx context.Context, addr string) {
	_, id := n.apiIPQuery(ctx, addr)
	if id == "" {
		n.sys.Config().Log.Printf("%s: %s: Failed to obtain IP address information", n.String(), addr)
		return
	}

	numRateLimitChecks(n, 3)
	asns := n.apiOrgInfoQuery(ctx, id)
	if len(asns) == 0 {
		n.sys.Config().Log.Printf("%s: %s: Failed to obtain ASNs associated with the organization", n.String(), id)
		return
	}

	var asn int
	cidrs := stringset.New()
	defer cidrs.Close()
	ip := net.ParseIP(addr)
loop:
	for _, a := range asns {
		numRateLimitChecks(n, 3)
		cidrs = n.apiNetblocksQuery(ctx, a)
		defer cidrs.Close()

		if cidrs.Len() == 0 {
			n.sys.Config().Log.Printf("%s: %d: Failed to obtain netblocks associated with the ASN", n.String(), a)
		}

		for _, cidr := range cidrs.Slice() {
			if _, ipnet, err := net.ParseCIDR(cidr); err == nil {
				if ipnet.Contains(ip) {
					asn = a
					break loop
				}
			}
		}
	}

	if asn == 0 {
		n.sys.Config().Log.Printf("%s: %s: Failed to obtain the ASN associated with the IP address", n.String(), addr)
		return
	}
	n.executeAPIASNQuery(ctx, asn, addr, cidrs)
}

func (n *NetworksDB) executeAPIASNQuery(ctx context.Context, asn int, addr string, netblocks *stringset.Set) {
	if netblocks == nil {
		netblocks = stringset.New()
		defer netblocks.Close()
	}

	if netblocks.Len() == 0 {
		set := n.apiNetblocksQuery(ctx, asn)
		defer set.Close()

		netblocks.Union(set)
		if netblocks.Len() == 0 {
			n.sys.Config().Log.Printf("%s: %d: Failed to obtain netblocks associated with the ASN", n.String(), asn)
			return
		}
	}

	var prefix string
	if addr != "" {
		ip := net.ParseIP(addr)
		for _, cidr := range netblocks.Slice() {
			if _, ipnet, err := net.ParseCIDR(cidr); err == nil && ipnet.Contains(ip) {
				prefix = cidr
				break
			}
		}
	}
	if prefix == "" {
		prefix = netblocks.Slice()[0]
	}

	numRateLimitChecks(n, 3)
	req := n.apiASNInfoQuery(ctx, asn)
	if req == nil {
		n.sys.Config().Log.Printf("%s: %d: Failed to obtain ASN information", n.String(), asn)
		return
	}

	if addr != "" {
		req.Address = addr
	}
	req.Prefix = prefix
	req.Netblocks = netblocks.Slice()
	n.sys.Cache().Update(req)
}

func (n *NetworksDB) apiIPQuery(ctx context.Context, addr string) (string, string) {
	numRateLimitChecks(n, 3)
	u := n.getAPIIPURL()
	params := url.Values{"ip": {addr}}
	body := strings.NewReader(params.Encode())
	page, err := http.RequestWebPage(ctx, u, body, n.getHeaders(), nil)
	if err != nil {
		n.sys.Config().Log.Printf("%s: %s: %v", n.String(), u, err)
		return "", ""
	}

	var m struct {
		Error   string `json:"error"`
		Total   int    `json:"total"`
		Results []struct {
			Org struct {
				ID string `json:"id"`
			} `json:"organisation"`
			Network struct {
				CIDR string `json:"cidr"`
			} `json:"network"`
		} `json:"results"`
	}
	if err := json.Unmarshal([]byte(page), &m); err != nil {
		n.sys.Config().Log.Printf("%s: %s: %v", n.String(), u, err)
		return "", ""
	} else if m.Error != "" {
		n.sys.Config().Log.Printf("%s: %s: %s", n.String(), u, m.Error)
		return "", ""
	} else if m.Total == 0 || len(m.Results) == 0 {
		n.sys.Config().Log.Printf("%s: %s: The request returned zero results", n.String(), u)
		return "", ""
	}

	return m.Results[0].Network.CIDR, m.Results[0].Org.ID
}

func (n *NetworksDB) getAPIIPURL() string {
	return networksdbBaseURL + networksdbAPIPATH + "/ip/info"
}

func (n *NetworksDB) apiOrgInfoQuery(ctx context.Context, id string) []int {
	numRateLimitChecks(n, 3)
	u := n.getAPIOrgInfoURL()
	params := url.Values{"id": {id}}
	body := strings.NewReader(params.Encode())
	page, err := http.RequestWebPage(ctx, u, body, n.getHeaders(), nil)
	if err != nil {
		n.sys.Config().Log.Printf("%s: %s: %v", n.String(), u, err)
		return []int{}
	}

	var m struct {
		Error   string `json:"error"`
		Total   int    `json:"total"`
		Results []struct {
			ASNs []int `json:"asns"`
		} `json:"results"`
	}
	if err := json.Unmarshal([]byte(page), &m); err != nil {
		n.sys.Config().Log.Printf("%s: %s: %v", n.String(), u, err)
		return []int{}
	} else if m.Error != "" {
		n.sys.Config().Log.Printf("%s: %s: %s", n.String(), u, m.Error)
		return []int{}
	} else if m.Total == 0 || len(m.Results[0].ASNs) == 0 {
		n.sys.Config().Log.Printf("%s: %s: The request returned zero results", n.String(), u)
		return []int{}
	}

	return m.Results[0].ASNs
}

func (n *NetworksDB) getAPIOrgInfoURL() string {
	return networksdbBaseURL + networksdbAPIPATH + "/org/info"
}

func (n *NetworksDB) apiASNInfoQuery(ctx context.Context, asn int) *requests.ASNRequest {
	numRateLimitChecks(n, 3)
	u := n.getAPIASNInfoURL()
	params := url.Values{"asn": {strconv.Itoa(asn)}}
	body := strings.NewReader(params.Encode())
	page, err := http.RequestWebPage(ctx, u, body, n.getHeaders(), nil)
	if err != nil {
		n.sys.Config().Log.Printf("%s: %s: %v", n.String(), u, err)
		return nil
	}

	var m struct {
		Error   string `json:"error"`
		Total   int    `json:"total"`
		Results []struct {
			ASN         int    `json:"asn"`
			ASName      string `json:"as_name"`
			Description string `json:"description"`
			CountryCode string `json:"countrycode"`
			Country     string `json:"country"`
		} `json:"results"`
	}
	if err := json.Unmarshal([]byte(page), &m); err != nil {
		n.sys.Config().Log.Printf("%s: %s: %v", n.String(), u, err)
		return nil
	} else if m.Error != "" {
		n.sys.Config().Log.Printf("%s: %s: %s", n.String(), u, m.Error)
		return nil
	} else if m.Total == 0 || len(m.Results) == 0 {
		n.sys.Config().Log.Printf("%s: %s: The request returned zero results", n.String(), u)
		return nil
	}

	return &requests.ASNRequest{
		ASN:         m.Results[0].ASN,
		CC:          m.Results[0].CountryCode,
		Description: m.Results[0].Description + ", " + m.Results[0].CountryCode,
		Tag:         n.SourceType,
		Source:      n.String(),
	}
}

func (n *NetworksDB) getAPIASNInfoURL() string {
	return networksdbBaseURL + networksdbAPIPATH + "/as/info"
}

func (n *NetworksDB) apiNetblocksQuery(ctx context.Context, asn int) *stringset.Set {
	netblocks := stringset.New()

	numRateLimitChecks(n, 3)
	u := n.getAPINetblocksURL()
	params := url.Values{"asn": {strconv.Itoa(asn)}}
	body := strings.NewReader(params.Encode())
	page, err := http.RequestWebPage(ctx, u, body, n.getHeaders(), nil)
	if err != nil {
		n.sys.Config().Log.Printf("%s: %s: %v", n.String(), u, err)
		return netblocks
	}

	var m struct {
		Error   string `json:"error"`
		Total   int    `json:"total"`
		Results []struct {
			CIDR string `json:"cidr"`
		} `json:"results"`
	}
	if err := json.Unmarshal([]byte(page), &m); err != nil {
		n.sys.Config().Log.Printf("%s: %s: %v", n.String(), u, err)
		return netblocks
	} else if m.Error != "" {
		n.sys.Config().Log.Printf("%s: %s: %s", n.String(), u, m.Error)
		return netblocks
	} else if m.Total == 0 || len(m.Results) == 0 {
		n.sys.Config().Log.Printf("%s: %s: The request returned zero results", n.String(), u)
		return netblocks
	}

	for _, block := range m.Results {
		netblocks.Insert(block.CIDR)
	}
	return netblocks
}

func (n *NetworksDB) getAPINetblocksURL() string {
	return networksdbBaseURL + networksdbAPIPATH + "/as/networks"
}

func (n *NetworksDB) getHeaders() map[string]string {
	if !n.hasAPIKey {
		return nil
	}

	return map[string]string{
		"X-Api-Key":    n.creds.Key,
		"Content-Type": "application/x-www-form-urlencoded",
	}
}

func (n *NetworksDB) whoisRequest(ctx context.Context, req *requests.WhoisRequest) {
	if !n.sys.Config().IsDomainInScope(req.Domain) {
		return
	}

	numRateLimitChecks(n, 2)
	u := n.getDomainToIPURL(req.Domain)
	page, err := http.RequestWebPage(ctx, u, nil, nil, nil)
	if err != nil {
		n.sys.Config().Log.Printf("%s: %s: %v", n.String(), u, err)
		return
	}

	matches := networksdbIPLinkRE.FindAllStringSubmatch(page, -1)
	if matches == nil {
		n.sys.Config().Log.Printf("%s: %s: Failed to extract the IP page href", n.String(), u)
		return
	}

	newdomains := stringset.New()
	defer newdomains.Close()

	re := dns.AnySubdomainRegex()
	for _, match := range matches {
		if len(match) < 2 {
			continue
		}

		numRateLimitChecks(n, 3)
		u = networksdbBaseURL + match[1]
		page, err = http.RequestWebPage(ctx, u, nil, nil, nil)
		if err != nil {
			n.sys.Config().Log.Printf("%s: %s: %v", n.String(), u, err)
			continue
		}

		cidrMatch := networksdbIPPageCIDRRE.FindStringSubmatch(page)
		if cidrMatch == nil || len(cidrMatch) < 2 {
			n.sys.Config().Log.Printf("%s: %s: Failed to extract the CIDR", n.String(), u)
			continue
		}

		_, cidr, err := net.ParseCIDR(cidrMatch[1])
		if err != nil {
			continue
		}

		numRateLimitChecks(n, 3)
		first, last := amassnet.FirstLast(cidr)
		u := n.getDomainsInNetworkURL(first.String(), last.String())

		page, err = http.RequestWebPage(ctx, u, nil, nil, nil)
		if err != nil {
			n.sys.Config().Log.Printf("%s: %s: %v", n.String(), u, err)
			continue
		}

		domainsPos := networksdbDomainsRE.FindStringIndex(page)
		tablePos := networksdbTableRE.FindStringIndex(page)
		if domainsPos == nil || tablePos == nil || len(domainsPos) < 2 || len(tablePos) < 2 {
			n.sys.Config().Log.Printf("%s: %s: Failed to extract the domain section of the page", n.String(), u)
			continue
		}

		start := domainsPos[1]
		end := tablePos[1]
		for _, d := range re.FindAllString(page[start:end], -1) {
			newdomains.Insert(strings.TrimSpace(d))
		}
	}

	if len(newdomains.Slice()) > 0 {
		n.Output() <- &requests.WhoisRequest{
			Domain:     req.Domain,
			NewDomains: newdomains.Slice(),
			Tag:        n.SourceType,
			Source:     n.String(),
		}
	}
}

func (n *NetworksDB) getDomainToIPURL(domain string) string {
	return networksdbBaseURL + "/domain-to-ips/" + domain
}

func (n *NetworksDB) getDomainsInNetworkURL(first, last string) string {
	return networksdbBaseURL + "/domains-in-network/" + first + "/" + last
}
