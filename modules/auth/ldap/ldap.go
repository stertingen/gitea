// Copyright 2014 The Gogs Authors. All rights reserved.
// Use of this source code is governed by a MIT-style
// license that can be found in the LICENSE file.

// Package ldap provide functions & structure to query a LDAP ldap directory
// For now, it's mainly tested again an MS Active Directory service, see README.md for more information
package ldap

import (
	"crypto/tls"
	"fmt"
	"strings"

	"code.gitea.io/gitea/modules/log"

	ldap "gopkg.in/ldap.v3"
)

// SecurityProtocol protocol type
type SecurityProtocol int

// Note: new type must be added at the end of list to maintain compatibility.
const (
	SecurityProtocolUnencrypted SecurityProtocol = iota
	SecurityProtocolLDAPS
	SecurityProtocolStartTLS
)

// Source Basic LDAP authentication service
type Source struct {
	Name                  string // canonical name (ie. corporate.ad)
	Host                  string // LDAP host
	Port                  int    // port number
	SecurityProtocol      SecurityProtocol
	SkipVerify            bool
	BindDN                string // DN to bind with
	BindPassword          string // Bind DN password
	UserBase              string // Base search path for users
	UserDN                string // Template for the DN of the user for simple auth
	GroupSearchBase       string // Base search path for groups
	GroupSearchFilter     string // Query group filter to validate entry
	UserAttributeInGroup  string // User attribute inserted into group filter
	MemberGroupFilter     string // Query group filter to check if user is allowed to log in
	AdminGroupFilter      string // Query group filter to check if user is admin
	AttributeUsername     string // Username attribute
	AttributeName         string // First name attribute
	AttributeSurname      string // Surname attribute
	AttributeMail         string // E-mail attribute
	AttributesInBind      bool   // fetch attributes in bind context (not user)
	AttributeSSHPublicKey string // LDAP SSH Public Key attribute
	SearchPageSize        uint32 // Search with paging page size
	Filter                string // Query filter to validate entry
	AdminFilter           string // Query filter to check if user is admin
	Enabled               bool   // if this source is disabled
}

// SearchResult : user data
type SearchResult struct {
	Username     string   // Username
	Name         string   // Name
	Surname      string   // Surname
	Mail         string   // E-mail address
	SSHPublicKey []string // SSH Public Key
	IsAdmin      bool     // if user is administrator
}

func (ls *Source) sanitizedUserQuery(username string) (string, bool) {
	// See http://tools.ietf.org/search/rfc4515
	badCharacters := "\x00()*\\"
	if strings.ContainsAny(username, badCharacters) {
		log.Debug("'%s' contains invalid query characters. Aborting.", username)
		return "", false
	}

	return fmt.Sprintf(ls.Filter, username), true
}

func (ls *Source) sanitizedGroupQuery(username string) (string, bool) {
	// See http://tools.ietf.org/search/rfc4515
	badCharacters := "\x00()*\\"
	if strings.ContainsAny(username, badCharacters) {
		log.Debug("'%s' contains invalid query characters. Aborting.", username)
		return "", false
	}

	return fmt.Sprintf(ls.GroupSearchFilter, username), true
}

func (ls *Source) sanitizedUserDN(username string) (string, bool) {
	// See http://tools.ietf.org/search/rfc4514: "special characters"
	badCharacters := "\x00()*\\,='\"#+;<>"
	if strings.ContainsAny(username, badCharacters) {
		log.Debug("'%s' contains invalid DN characters. Aborting.", username)
		return "", false
	}

	return fmt.Sprintf(ls.UserDN, username), true
}

func (ls *Source) findUserDN(l *ldap.Conn, name string) (string, bool) {
	log.Trace("Search for LDAP user: %s", name)

	// A search for the user.
	userFilter, ok := ls.sanitizedUserQuery(name)
	if !ok {
		return "", false
	}

	log.Trace("Searching for DN using filter %s and base %s", userFilter, ls.UserBase)
	search := ldap.NewSearchRequest(
		ls.UserBase, ldap.ScopeWholeSubtree, ldap.NeverDerefAliases, 0, 0,
		false, userFilter, []string{}, nil)

	// Ensure we found a user
	sr, err := l.Search(search)
	if err != nil || len(sr.Entries) < 1 {
		log.Debug("Failed search using filter[%s]: %v", userFilter, err)
		return "", false
	} else if len(sr.Entries) > 1 {
		log.Debug("Filter '%s' returned more than one user.", userFilter)
		return "", false
	}

	userDN := sr.Entries[0].DN
	if userDN == "" {
		log.Error("LDAP search was successful, but found no DN!")
		return "", false
	}

	return userDN, true
}

func dial(ls *Source) (*ldap.Conn, error) {
	log.Trace("Dialing LDAP with security protocol (%v) without verifying: %v", ls.SecurityProtocol, ls.SkipVerify)

	tlsCfg := &tls.Config{
		ServerName:         ls.Host,
		InsecureSkipVerify: ls.SkipVerify,
	}
	if ls.SecurityProtocol == SecurityProtocolLDAPS {
		return ldap.DialTLS("tcp", fmt.Sprintf("%s:%d", ls.Host, ls.Port), tlsCfg)
	}

	conn, err := ldap.Dial("tcp", fmt.Sprintf("%s:%d", ls.Host, ls.Port))
	if err != nil {
		return nil, fmt.Errorf("Dial: %v", err)
	}

	if ls.SecurityProtocol == SecurityProtocolStartTLS {
		if err = conn.StartTLS(tlsCfg); err != nil {
			conn.Close()
			return nil, fmt.Errorf("StartTLS: %v", err)
		}
	}

	return conn, nil
}

func bindUser(l *ldap.Conn, userDN, passwd string) error {
	log.Trace("Binding with userDN: %s", userDN)
	err := l.Bind(userDN, passwd)
	if err != nil {
		log.Debug("LDAP auth. failed for %s, reason: %v", userDN, err)
		return err
	}
	log.Trace("Bound successfully with userDN: %s", userDN)
	return err
}

func checkAdmin(l *ldap.Conn, ls *Source, userDN string) bool {
	if len(ls.AdminFilter) > 0 {
		log.Trace("Checking admin with filter %s and base %s", ls.AdminFilter, userDN)
		search := ldap.NewSearchRequest(
			userDN, ldap.ScopeWholeSubtree, ldap.NeverDerefAliases, 0, 0, false, ls.AdminFilter,
			[]string{ls.AttributeName},
			nil)

		sr, err := l.Search(search)

		if err != nil {
			log.Error("LDAP Admin Search failed unexpectedly! (%v)", err)
		} else if len(sr.Entries) < 1 {
			log.Trace("LDAP Admin Search found no matching entries.")
		} else {
			return true
		}
	}
	return false
}

// CheckGroupFilter :
func (ls *Source) CheckGroupFilter(l *ldap.Conn, groupSR *ldap.SearchResult, filter string) bool {
	for _, groupEntry := range groupSR.Entries {
		search := ldap.NewSearchRequest(groupEntry.DN, ldap.ScopeBaseObject, ldap.NeverDerefAliases, 0, 0, false, filter, []string{}, nil)
		sr, err := l.Search(search)
		if (err == nil) && (len(sr.Entries) > 0) {
			return true
		}
	}
	log.Trace("LDAP group search with filter %v found no matching entries", filter)
	return false
}

// SearchEntry : search an LDAP source if an entry (name, passwd) is valid and in the specific filter
func (ls *Source) SearchEntry(name, passwd string, directBind bool) *SearchResult {
	// See https://tools.ietf.org/search/rfc4513#section-5.1.2
	if len(passwd) == 0 {
		log.Debug("Auth. failed for %s, password cannot be empty", name)
		return nil
	}
	l, err := dial(ls)
	if err != nil {
		log.Error("LDAP Connect error, %s:%v", ls.Host, err)
		ls.Enabled = false
		return nil
	}
	defer l.Close()

	var userDN string
	if directBind {
		log.Trace("LDAP will bind directly via UserDN template: %s", ls.UserDN)

		var ok bool
		userDN, ok = ls.sanitizedUserDN(name)

		if !ok {
			return nil
		}

		err = bindUser(l, userDN, passwd)
		if err != nil {
			return nil
		}

		if ls.UserBase != "" {
			// not everyone has a CN compatible with input name so we need to find
			// the real userDN in that case

			userDN, ok = ls.findUserDN(l, name)
			if !ok {
				return nil
			}
		}
	} else {
		log.Trace("LDAP will use BindDN.")

		var found bool

		if ls.BindDN != "" && ls.BindPassword != "" {
			err := l.Bind(ls.BindDN, ls.BindPassword)
			if err != nil {
				log.Debug("Failed to bind as BindDN[%s]: %v", ls.BindDN, err)
				return nil
			}
			log.Trace("Bound as BindDN %s", ls.BindDN)
		} else {
			log.Trace("Proceeding with anonymous LDAP search.")
		}

		userDN, found = ls.findUserDN(l, name)
		if !found {
			return nil
		}
	}

	if !ls.AttributesInBind {
		// binds user (checking password) before looking-up attributes in user context
		err = bindUser(l, userDN, passwd)
		if err != nil {
			return nil
		}
	}

	userFilter, ok := ls.sanitizedUserQuery(name)
	if !ok {
		return nil
	}

	var isAttributeSSHPublicKeySet = len(strings.TrimSpace(ls.AttributeSSHPublicKey)) > 0

	attribs := []string{ls.AttributeUsername, ls.AttributeName, ls.AttributeSurname, ls.AttributeMail, ls.UserAttributeInGroup}
	if isAttributeSSHPublicKeySet {
		attribs = append(attribs, ls.AttributeSSHPublicKey)
	}

	log.Trace("Fetching attributes '%v', '%v', '%v', '%v', '%v', '%v' with filter %s and base %s", ls.AttributeUsername, ls.AttributeName, ls.AttributeSurname, ls.AttributeMail, ls.AttributeSSHPublicKey, ls.UserAttributeInGroup, userFilter, userDN)
	search := ldap.NewSearchRequest(
		userDN, ldap.ScopeWholeSubtree, ldap.NeverDerefAliases, 0, 0, false, userFilter,
		attribs, nil)

	sr, err := l.Search(search)
	if err != nil {
		log.Error("LDAP Search failed unexpectedly! (%v)", err)
		return nil
	} else if len(sr.Entries) < 1 {
		if directBind {
			log.Trace("User filter inhibited user login.")
		} else {
			log.Trace("LDAP Search found no matching entries.")
		}

		return nil
	}

	var sshPublicKey []string

	username := sr.Entries[0].GetAttributeValue(ls.AttributeUsername)
	firstname := sr.Entries[0].GetAttributeValue(ls.AttributeName)
	surname := sr.Entries[0].GetAttributeValue(ls.AttributeSurname)
	mail := sr.Entries[0].GetAttributeValue(ls.AttributeMail)
	if isAttributeSSHPublicKeySet {
		sshPublicKey = sr.Entries[0].GetAttributeValues(ls.AttributeSSHPublicKey)
	}

	var hasAdminGroup = false
	if len(strings.TrimSpace(ls.GroupSearchBase)) > 0 && len(strings.TrimSpace(ls.GroupSearchFilter)) > 0 {
		var groupUID string
		if len(strings.TrimSpace(ls.UserAttributeInGroup)) > 0 {
			groupUID = sr.Entries[0].GetAttributeValue(ls.UserAttributeInGroup)
		} else {
			groupUID = sr.Entries[0].DN
		}
		log.Trace("User attribute used in LDAP group: %v", groupUID)

		groupFilter, ok := ls.sanitizedGroupQuery(groupUID)
		if !ok {
			return nil
		}

		groupSearch := ldap.NewSearchRequest(
			ls.GroupSearchBase, ldap.ScopeWholeSubtree, ldap.NeverDerefAliases, 0, 0, false, groupFilter, []string{}, nil)

		sr, err := l.Search(groupSearch)
		if err != nil {
			log.Error("LDAP group search failed unexpectedly! (%v)", err)
			return nil
		}

		if len(strings.TrimSpace(ls.MemberGroupFilter)) > 0 {
			if !ls.CheckGroupFilter(l, sr, ls.MemberGroupFilter) {
				log.Error("No group matched the required member group filter!")
				return nil
			}
		}

		if len(strings.TrimSpace(ls.AdminGroupFilter)) > 0 {
			hasAdminGroup = ls.CheckGroupFilter(l, sr, ls.AdminGroupFilter)
			log.Info("LDAP user is in admin group!")
		}
	}

	isAdmin := hasAdminGroup || checkAdmin(l, ls, userDN)

	if !directBind && ls.AttributesInBind {
		// binds user (checking password) after looking-up attributes in BindDN context
		err = bindUser(l, userDN, passwd)
		if err != nil {
			return nil
		}
	}

	return &SearchResult{
		Username:     username,
		Name:         firstname,
		Surname:      surname,
		Mail:         mail,
		SSHPublicKey: sshPublicKey,
		IsAdmin:      isAdmin,
	}
}

// UsePagedSearch returns if need to use paged search
func (ls *Source) UsePagedSearch() bool {
	return ls.SearchPageSize > 0
}

// SearchEntries : search an LDAP source for all users matching userFilter
func (ls *Source) SearchEntries() ([]*SearchResult, error) {
	l, err := dial(ls)
	if err != nil {
		log.Error("LDAP Connect error, %s:%v", ls.Host, err)
		ls.Enabled = false
		return nil, err
	}
	defer l.Close()

	if ls.BindDN != "" && ls.BindPassword != "" {
		err := l.Bind(ls.BindDN, ls.BindPassword)
		if err != nil {
			log.Debug("Failed to bind as BindDN[%s]: %v", ls.BindDN, err)
			return nil, err
		}
		log.Trace("Bound as BindDN %s", ls.BindDN)
	} else {
		log.Trace("Proceeding with anonymous LDAP search.")
	}

	userFilter := fmt.Sprintf(ls.Filter, "*")

	var isAttributeSSHPublicKeySet = len(strings.TrimSpace(ls.AttributeSSHPublicKey)) > 0

	attribs := []string{ls.AttributeUsername, ls.AttributeName, ls.AttributeSurname, ls.AttributeMail}
	if isAttributeSSHPublicKeySet {
		attribs = append(attribs, ls.AttributeSSHPublicKey)
	}

	log.Trace("Fetching attributes '%v', '%v', '%v', '%v', '%v' with filter %s and base %s", ls.AttributeUsername, ls.AttributeName, ls.AttributeSurname, ls.AttributeMail, ls.AttributeSSHPublicKey, userFilter, ls.UserBase)
	search := ldap.NewSearchRequest(
		ls.UserBase, ldap.ScopeWholeSubtree, ldap.NeverDerefAliases, 0, 0, false, userFilter,
		attribs, nil)

	var sr *ldap.SearchResult
	if ls.UsePagedSearch() {
		sr, err = l.SearchWithPaging(search, ls.SearchPageSize)
	} else {
		sr, err = l.Search(search)
	}
	if err != nil {
		log.Error("LDAP Search failed unexpectedly! (%v)", err)
		return nil, err
	}

	results := []*SearchResult{}

	for _, v := range sr.Entries {

		// TODO: Remove code duplication
		var hasAdminGroup = false
		if len(strings.TrimSpace(ls.GroupSearchBase)) > 0 && len(strings.TrimSpace(ls.GroupSearchFilter)) > 0 {
			var groupUID string
			if len(strings.TrimSpace(ls.UserAttributeInGroup)) > 0 {
				groupUID = v.GetAttributeValue(ls.UserAttributeInGroup)
			} else {
				groupUID = v.DN
			}
			log.Trace("User attribute used in LDAP group: %v", groupUID)

			groupFilter, ok := ls.sanitizedGroupQuery(groupUID)
			if !ok {
				continue
			}

			groupSearch := ldap.NewSearchRequest(
				ls.GroupSearchBase, ldap.ScopeWholeSubtree, ldap.NeverDerefAliases, 0, 0, false, groupFilter, []string{}, nil)

			sr, err := l.Search(groupSearch)
			if err != nil {
				log.Error("LDAP group search failed unexpectedly! (%v)", err)
				continue
			}

			if len(strings.TrimSpace(ls.MemberGroupFilter)) > 0 {
				if !ls.CheckGroupFilter(l, sr, ls.MemberGroupFilter) {
					log.Error("No group matched the required member group filter!")
					continue
				}
			}

			if len(strings.TrimSpace(ls.AdminGroupFilter)) > 0 {
				hasAdminGroup = ls.CheckGroupFilter(l, sr, ls.AdminGroupFilter)
				log.Info("LDAP user is in admin group!")
			}
		}

		result := &SearchResult{
			Username: v.GetAttributeValue(ls.AttributeUsername),
			Name:     v.GetAttributeValue(ls.AttributeName),
			Surname:  v.GetAttributeValue(ls.AttributeSurname),
			Mail:     v.GetAttributeValue(ls.AttributeMail),
			IsAdmin:  hasAdminGroup || checkAdmin(l, ls, v.DN),
		}
		if isAttributeSSHPublicKeySet {
			result.SSHPublicKey = v.GetAttributeValues(ls.AttributeSSHPublicKey)
		}
		results = append(results, result)
	}

	return results, nil
}
