// Copyright © by Jeff Foley 2017-2022. All rights reserved.
// Use of this source code is governed by Apache 2 LICENSE that can be found in the LICENSE file.
// SPDX-License-Identifier: Apache-2.0

package datasrcs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/aokimio/Amass/v3/config"
	"github.com/aokimio/Amass/v3/net/http"
	"github.com/aokimio/Amass/v3/requests"
	"github.com/aokimio/Amass/v3/systems"
	"github.com/caffix/resolve"
	"github.com/caffix/service"
	"github.com/caffix/stringset"
)

// Umbrella is the Service that handles access to the Umbrella data source.
type Umbrella struct {
	service.BaseService

	SourceType string
	sys        systems.System
	creds      *config.Credentials
}

// NewUmbrella returns he object initialized, but not yet started.
func NewUmbrella(sys systems.System) *Umbrella {
	u := &Umbrella{
		SourceType: requests.API,
		sys:        sys,
	}

	go u.requests()
	u.BaseService = *service.NewBaseService(u, "Umbrella")
	return u
}

// Description implements the Service interface.
func (u *Umbrella) Description() string {
	return u.SourceType
}

// OnStart implements the Service interface.
func (u *Umbrella) OnStart() error {
	u.creds = u.sys.Config().GetDataSourceConfig(u.String()).GetCredentials()

	if u.creds == nil || u.creds.Key == "" {
		u.sys.Config().Log.Printf("%s: API key data was not provided", u.String())
	}

	u.SetRateLimit(2)
	return u.checkConfig()
}

// CheckConfig implements the Service interface.
func (u *Umbrella) checkConfig() error {
	creds := u.sys.Config().GetDataSourceConfig(u.String()).GetCredentials()

	if creds == nil || creds.Key == "" {
		estr := fmt.Sprintf("%s: check callback failed for the configuration", u.String())
		u.sys.Config().Log.Print(estr)
		return errors.New(estr)
	}

	return nil
}

func (u *Umbrella) requests() {
	for {
		select {
		case <-u.Done():
			return
		case in := <-u.Input():
			switch req := in.(type) {
			case *requests.DNSRequest:
				u.CheckRateLimit()
				u.dnsRequest(context.TODO(), req)
			case *requests.AddrRequest:
				u.CheckRateLimit()
				u.addrRequest(context.TODO(), req)
			case *requests.ASNRequest:
				u.CheckRateLimit()
				u.asnRequest(context.TODO(), req)
			case *requests.WhoisRequest:
				u.CheckRateLimit()
				u.whoisRequest(context.TODO(), req)
			}
		}
	}
}

func (u *Umbrella) dnsRequest(ctx context.Context, req *requests.DNSRequest) {
	if u.creds == nil || u.creds.Key == "" {
		return
	}
	if !u.sys.Config().IsDomainInScope(req.Domain) {
		return
	}

	u.sys.Config().Log.Printf("Querying %s for %s subdomains", u.String(), req.Domain)

	headers := u.restHeaders()
	url := u.restDNSURL(req.Domain)
	page, err := http.RequestWebPage(ctx, url, nil, headers, nil)
	if err != nil {
		u.sys.Config().Log.Printf("%s: %s: %v", u.String(), url, err)
		return
	}
	// Extract the subdomain names from the REST API results
	var subs struct {
		Matches []struct {
			Name string `json:"name"`
		} `json:"matches"`
	}
	if err := json.Unmarshal([]byte(page), &subs); err != nil {
		return
	}
	for _, m := range subs.Matches {
		genNewNameEvent(ctx, u.sys, u, m.Name)
	}
}

func (u *Umbrella) addrRequest(ctx context.Context, req *requests.AddrRequest) {
	if u.creds == nil || u.creds.Key == "" {
		return
	}
	if req.Address == "" {
		return
	}

	headers := u.restHeaders()
	url := u.restAddrURL(req.Address)
	page, err := http.RequestWebPage(ctx, url, nil, headers, nil)
	if err != nil {
		u.sys.Config().Log.Printf("%s: %s: %v", u.String(), url, err)
		return
	}
	// Extract the subdomain names from the REST API results
	var ip struct {
		Records []struct {
			Data string `json:"rr"`
		} `json:"records"`
	}
	if err := json.Unmarshal([]byte(page), &ip); err != nil {
		return
	}

	for _, record := range ip.Records {
		if name := resolve.RemoveLastDot(record.Data); name != "" {
			genNewNameEvent(ctx, u.sys, u, name)
		}
	}
}

func (u *Umbrella) asnRequest(ctx context.Context, req *requests.ASNRequest) {
	if u.creds == nil || u.creds.Key == "" {
		return
	}
	if req.Address == "" && req.ASN == 0 {
		return
	}
	if req.Address != "" {
		u.executeASNAddrQuery(ctx, req)
		return
	}
	u.executeASNQuery(ctx, req)
}

func (u *Umbrella) executeASNAddrQuery(ctx context.Context, req *requests.ASNRequest) {
	headers := u.restHeaders()
	url := u.restAddrToASNURL(req.Address)
	page, err := http.RequestWebPage(ctx, url, nil, headers, nil)
	if err != nil {
		u.sys.Config().Log.Printf("%s: %s: %v", u.String(), url, err)
		return
	}
	// Extract the AS information from the REST API results
	var as []struct {
		Date        string `json:"creation_date"`
		Registry    int    `json:"ir"`
		Description string `json:"description"`
		ASN         int    `json:"asn"`
		CIDR        string `json:"cidr"`
	}
	if err := json.Unmarshal([]byte(page), &as); err != nil || len(as) == 0 {
		return
	}

	created, err := time.Parse("2006-01-02", as[0].Date)
	if err != nil {
		return
	}

	var registry string
	switch as[0].Registry {
	case 1:
		registry = "AfriNIC"
	case 2:
		registry = "APNIC"
	case 3:
		registry = "ARIN"
	case 4:
		registry = "LACNIC"
	case 5:
		registry = "RIPE NCC"
	default:
		registry = "N/A"
	}

	req.ASN = as[0].ASN
	req.Prefix = as[0].CIDR
	req.Registry = registry
	req.AllocationDate = created
	req.Description = as[0].Description
	req.Tag = u.SourceType
	req.Source = u.String()
	if len(req.Netblocks) == 0 {
		req.Netblocks = []string{strings.TrimSpace(req.Prefix)}

		u.CheckRateLimit()
		u.executeASNQuery(ctx, req)
	}

	u.sys.Cache().Update(req)
}

func (u *Umbrella) executeASNQuery(ctx context.Context, req *requests.ASNRequest) {
	headers := u.restHeaders()
	url := u.restASNToCIDRsURL(req.ASN)
	page, err := http.RequestWebPage(ctx, url, nil, headers, nil)
	if err != nil {
		u.sys.Config().Log.Printf("%s: %s: %v", u.String(), url, err)
		return
	}
	// Extract the netblock information from the REST API results
	var netblock []struct {
		CIDR string `json:"cidr"`
		Geo  struct {
			CountryName string `json:"country_name"`
			CountryCode string `json:"country_code"`
		} `json:"geo"`
	}
	if err := json.Unmarshal([]byte(page), &netblock); err != nil || len(netblock) == 0 {
		return
	}

	for _, nb := range netblock {
		req.Netblocks = append(req.Netblocks, strings.TrimSpace(nb.CIDR))
		if nb.CIDR == req.Prefix {
			req.CC = nb.Geo.CountryCode
		}
	}
	// If no basic AS info exists, then obtain an IP and query
	if req.Prefix == "" {
		addr, _, err := net.ParseCIDR(netblock[0].CIDR)

		if err == nil {
			req.Address = addr.String()
			req.CC = netblock[0].Geo.CountryCode

			u.CheckRateLimit()
			u.executeASNAddrQuery(ctx, req)
			return
		}
	}
	// Finish populating the AS info in the request
	for _, nb := range netblock {
		if nb.CIDR == req.Prefix {
			req.CC = nb.Geo.CountryCode
			break
		}
	}
}

// Umbrella provides much more than this, but we're only interested in these
// fields
type whoisRecord struct {
	NameServers         []string `json:"nameServers"`
	AdminContactEmail   string   `json:"administrativeContactEmail"`
	BillingContactEmail string   `json:"billingContactEmail"`
	RegistrantEmail     string   `json:"registrantEmail"`
	TechContactEmail    string   `json:"technicalContactEmail"`
	ZoneContactEmail    string   `json:"zoneContactEmail"`
}

// Umbrella provides the same response for email and ns reverse records. Makes
// the json parsing logic simple since we can use the same structs for both
type rWhoisDomain struct {
	Domain  string `json:"domain"`
	Current bool   `json:"current"`
}

type rWhoisResponse struct {
	TotalResults int            `json:"totalResults"`
	MoreData     bool           `json:"moreDataAvailable"`
	Limit        int            `json:"limit"`
	Domains      []rWhoisDomain `json:"domains"`
}

func (u *Umbrella) collateEmails(ctx context.Context, record *whoisRecord) []string {
	emails := stringset.New()
	defer emails.Close()

	if u.validateScope(ctx, record.AdminContactEmail) {
		emails.InsertMany(record.AdminContactEmail)
	}
	if u.validateScope(ctx, record.BillingContactEmail) {
		emails.InsertMany(record.BillingContactEmail)
	}
	if u.validateScope(ctx, record.RegistrantEmail) {
		emails.InsertMany(record.RegistrantEmail)
	}
	if u.validateScope(ctx, record.TechContactEmail) {
		emails.InsertMany(record.TechContactEmail)
	}
	if u.validateScope(ctx, record.ZoneContactEmail) {
		emails.InsertMany(record.ZoneContactEmail)
	}
	return emails.Slice()
}

func (u *Umbrella) queryWhois(ctx context.Context, domain string) *whoisRecord {
	var whois whoisRecord
	headers := u.restHeaders()
	whoisURL := u.whoisRecordURL(domain)

	u.CheckRateLimit()
	record, err := http.RequestWebPage(ctx, whoisURL, nil, headers, nil)
	if err != nil {
		u.sys.Config().Log.Printf("%s: %s: %v", u.String(), whoisURL, err)
		return nil
	}

	err = json.Unmarshal([]byte(record), &whois)
	if err != nil {
		u.sys.Config().Log.Printf("%s: %s: %v", u.String(), whoisURL, err)
		return nil
	}
	return &whois
}

func (u *Umbrella) queryReverseWhois(ctx context.Context, apiURL string) []string {
	domains := stringset.New()
	defer domains.Close()

	headers := u.restHeaders()
	var whois map[string]rWhoisResponse
	// Umbrella provides data in 500 piece chunks
	for count, more := 0, true; more; count = count + 500 {
		u.CheckRateLimit()
		fullAPIURL := fmt.Sprintf("%s&offset=%d", apiURL, count)
		record, err := http.RequestWebPage(ctx, fullAPIURL, nil, headers, nil)
		if err != nil {
			u.sys.Config().Log.Printf("%s: %s: %v", u.String(), apiURL, err)
			return domains.Slice()
		}

		err = json.Unmarshal([]byte(record), &whois)
		if err != nil {
			u.sys.Config().Log.Printf("%s: %s: %v", u.String(), apiURL, err)
			return domains.Slice()
		}

		more = false
		for _, result := range whois {
			if result.TotalResults > 0 {
				for _, domain := range result.Domains {
					if domain.Current {
						domains.Insert(domain.Domain)
					}
				}
			}
			if result.MoreData && !more {
				more = true
			}
		}
	}
	return domains.Slice()
}

func (u *Umbrella) validateScope(ctx context.Context, input string) bool {
	if input != "" && u.sys.Config().IsDomainInScope(input) {
		return true
	}
	return false
}

func (u *Umbrella) whoisRequest(ctx context.Context, req *requests.WhoisRequest) {
	if u.creds == nil || u.creds.Key == "" {
		return
	}
	if !u.sys.Config().IsDomainInScope(req.Domain) {
		return
	}

	whoisRecord := u.queryWhois(ctx, req.Domain)
	if whoisRecord == nil {
		return
	}

	domains := stringset.New()
	defer domains.Close()

	emails := u.collateEmails(ctx, whoisRecord)
	if len(emails) > 0 {
		emailURL := u.reverseWhoisByEmailURL(emails...)
		for _, d := range u.queryReverseWhois(ctx, emailURL) {
			if !u.sys.Config().IsDomainInScope(d) {
				domains.Insert(d)
			}
		}
	}

	var nameservers []string
	for _, ns := range whoisRecord.NameServers {
		if u.validateScope(ctx, ns) {
			nameservers = append(nameservers, ns)
		}
	}
	if len(nameservers) > 0 {
		nsURL := u.reverseWhoisByNSURL(nameservers...)
		for _, d := range u.queryReverseWhois(ctx, nsURL) {
			if !u.sys.Config().IsDomainInScope(d) {
				domains.Insert(d)
			}
		}
	}

	if domains.Len() > 0 {
		u.Output() <- &requests.WhoisRequest{
			Domain:     req.Domain,
			NewDomains: domains.Slice(),
			Tag:        u.SourceType,
			Source:     u.String(),
		}
	}
}

func (u *Umbrella) restHeaders() map[string]string {
	headers := map[string]string{"Content-Type": "application/json"}

	if u.creds != nil && u.creds.Key != "" {
		headers["Authorization"] = "Bearer " + u.creds.Key
	}

	return headers
}

func (u *Umbrella) whoisBaseURL() string {
	return `https://investigate.api.umbrella.com/whois/`
}

func (u *Umbrella) whoisRecordURL(domain string) string {
	return u.whoisBaseURL() + domain
}

func (u *Umbrella) reverseWhoisByNSURL(ns ...string) string {
	nameservers := strings.Join(ns, ",")

	return u.whoisBaseURL() + `nameservers?nameServerList=` + nameservers
}

func (u *Umbrella) reverseWhoisByEmailURL(emails ...string) string {
	emailQuery := strings.Join(emails, ",")

	return u.whoisBaseURL() + `emails?emailList=` + emailQuery
}

func (u *Umbrella) restDNSURL(domain string) string {
	return `https://investigate.api.umbrella.com/search/.*[.]` + domain + "?start=-30days&limit=1000"
}

func (u *Umbrella) restAddrURL(addr string) string {
	return "https://investigate.api.umbrella.com/pdns/ip/" + addr + "?recordType=A,AAAA"
}

func (u *Umbrella) restAddrToASNURL(addr string) string {
	return fmt.Sprintf("https://investigate.api.umbrella.com/bgp_routes/ip/%s/as_for_ip.json", addr)
}

func (u *Umbrella) restASNToCIDRsURL(asn int) string {
	return fmt.Sprintf("https://investigate.api.umbrella.com/bgp_routes/asn/%d/prefixes_for_asn.json", asn)
}
