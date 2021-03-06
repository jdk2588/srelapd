package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"net"
	"sort"
	"strings"

	"github.com/pquerna/otp/totp"
	"github.com/tsocial/srelapd/ldap"
)

type configHandler struct {
	cfg *config
}

func newConfigHandler(cfg *config) Backend {
	handler := configHandler{cfg: cfg}
	return handler
}

//
func (h configHandler) Bind(bindDN, bindSimplePw string, conn net.Conn) (resultCode ldap.LDAPResultCode, err error) {
	bindDN = strings.ToLower(bindDN)
	baseDN := strings.ToLower("," + h.cfg.BaseDN)

	log.Printf(
		"Bind request: bindDN: %s, BaseDN: %s, source: %s\n",
		bindDN, h.cfg.BaseDN, conn.RemoteAddr().String(),
	)

	// parse the bindDN - ensure that the bindDN ends with the BaseDN
	if !strings.HasSuffix(bindDN, baseDN) {
		log.Printf("Bind Error: BindDN %s not our BaseDN %s\n", bindDN, h.cfg.BaseDN)
		return ldap.LDAPResultInvalidCredentials, nil
	}
	parts := strings.Split(strings.TrimSuffix(bindDN, baseDN), ",")
	groupName := ""
	userName := ""
	if len(parts) == 1 {
		userName = strings.TrimPrefix(parts[0], "cn=")
	} else if len(parts) == 2 {
		userName = strings.TrimPrefix(parts[0], "cn=")
		groupName = strings.TrimPrefix(parts[1], "ou=")
	} else {
		log.Printf(
			"Bind Error: BindDN %s should have only one or two parts (has %d)\n",
			bindDN, len(parts),
		)

		return ldap.LDAPResultInvalidCredentials, nil
	}
	// find the user
	user := configUser{}
	found := false
	for _, u := range h.cfg.Users {
		if u.Name == userName {
			found = true
			user = u
		}
	}
	if !found {
		log.Printf("Bind Error: User %s not found\n", userName)
		return ldap.LDAPResultInvalidCredentials, nil
	}
	// find the group
	group := configGroup{}
	found = false
	for _, g := range h.cfg.Groups {
		if g.Name == groupName {
			found = true
			group = g
		}
	}
	if !found {
		log.Printf("Bind Error: Group %s not found\n", groupName)
		return ldap.LDAPResultInvalidCredentials, nil
	}
	// validate group membership
	if user.PrimaryGroup != group.UnixID {
		log.Printf("Bind Error: User %s primary group is not %s\n", userName, groupName)
		return ldap.LDAPResultInvalidCredentials, nil
	}

	validotp := false

	if len(user.OTPSecret) == 0 {
		validotp = true
	}

	if len(user.OTPSecret) > 0 && !validotp {
		if len(bindSimplePw) > 6 {
			otp := bindSimplePw[len(bindSimplePw)-6:]
			bindSimplePw = bindSimplePw[:len(bindSimplePw)-6]

			validotp = totp.Validate(otp, user.OTPSecret)
		}
	}

	if !validotp {
		log.Printf(
			"Bind Error: invalid token as %s from %s\n",
			bindDN, conn.RemoteAddr().String(),
		)

		return ldap.LDAPResultInvalidCredentials, nil
	}

	// finally, validate user's pw
	hash := sha256.New()
	hash.Write([]byte(bindSimplePw))
	if user.PassSHA256 != hex.EncodeToString(hash.Sum(nil)) {
		log.Printf(
			"Bind Error: invalid credentials as %s from %s\n",
			bindDN, conn.RemoteAddr().String(),
		)

		return ldap.LDAPResultInvalidCredentials, nil
	}
	log.Printf("Bind success as %s from %s\n", bindDN, conn.RemoteAddr().String())
	return ldap.LDAPResultSuccess, nil
}

//
func (h configHandler) Search(bindDN string, searchReq ldap.SearchRequest, conn net.Conn) (result ldap.ServerSearchResult, err error) {
	bindDN = strings.ToLower(bindDN)
	baseDN := strings.ToLower("," + h.cfg.BaseDN)
	searchBaseDN := strings.ToLower(searchReq.BaseDN)
	log.Printf(
		"Search request as %s from %s for %s\n",
		bindDN, conn.RemoteAddr().String(), searchReq.Filter,
	)

	// validate the user is authenticated and has appropriate access
	if len(bindDN) < 1 {
		return ldap.ServerSearchResult{ResultCode: ldap.LDAPResultInsufficientAccessRights}, fmt.Errorf("Search Error: Anonymous BindDN not allowed %s", bindDN)
	}
	if !strings.HasSuffix(bindDN, baseDN) {
		return ldap.ServerSearchResult{ResultCode: ldap.LDAPResultInsufficientAccessRights}, fmt.Errorf("Search Error: BindDN %s not in our BaseDN %s", bindDN, h.cfg.BaseDN)
	}
	if !strings.HasSuffix(searchBaseDN, h.cfg.BaseDN) {
		return ldap.ServerSearchResult{ResultCode: ldap.LDAPResultInsufficientAccessRights}, fmt.Errorf("Search Error: search BaseDN %s is not in our BaseDN %s", searchBaseDN, h.cfg.BaseDN)
	}
	// return all users in the config file - the LDAP library will filter results for us
	entries := []*ldap.Entry{}
	filterEntity, err := ldap.GetFilterObjectClass(searchReq.Filter)
	if err != nil {
		return ldap.ServerSearchResult{ResultCode: ldap.LDAPResultOperationsError}, fmt.Errorf("Search Error: error parsing filter: %s", searchReq.Filter)
	}
	switch filterEntity {
	default:
		return ldap.ServerSearchResult{ResultCode: ldap.LDAPResultOperationsError}, fmt.Errorf("Search Error: unhandled filter type: %s [%s]", filterEntity, searchReq.Filter)
	case "posixgroup":
		for _, g := range h.cfg.Groups {
			attrs := []*ldap.EntryAttribute{}
			attrs = append(attrs, &ldap.EntryAttribute{"cn", []string{g.Name}})
			attrs = append(attrs, &ldap.EntryAttribute{"description", []string{fmt.Sprintf("%s via LDAP", g.Name)}})
			attrs = append(attrs, &ldap.EntryAttribute{"gidNumber", []string{fmt.Sprintf("%d", g.UnixID)}})
			attrs = append(attrs, &ldap.EntryAttribute{"objectClass", []string{"posixGroup"}})
			attrs = append(attrs, &ldap.EntryAttribute{"uniqueMember", h.getGroupMembers(g.UnixID)})
			attrs = append(attrs, &ldap.EntryAttribute{"memberUid", h.getGroupMemberIDs(g.UnixID)})
			dn := fmt.Sprintf("cn=%s,ou=groups,%s", g.Name, h.cfg.BaseDN)
			entries = append(entries, &ldap.Entry{dn, attrs})
		}
	case "posixaccount", "":
		for _, u := range h.cfg.Users {
			attrs := []*ldap.EntryAttribute{}
			attrs = append(attrs, &ldap.EntryAttribute{"cn", []string{u.Name}})
			attrs = append(attrs, &ldap.EntryAttribute{"uid", []string{u.Name}})

			if len(u.GivenName) > 0 {
				attrs = append(attrs, &ldap.EntryAttribute{"givenName", []string{u.GivenName}})
			}

			if len(u.SN) > 0 {
				attrs = append(attrs, &ldap.EntryAttribute{"sn", []string{u.SN}})
			}

			attrs = append(attrs, &ldap.EntryAttribute{"ou", []string{h.getGroupName(u.PrimaryGroup)}})
			attrs = append(attrs, &ldap.EntryAttribute{"uidNumber", []string{fmt.Sprintf("%d", u.UnixID)}})

			if u.Disabled {
				attrs = append(attrs, &ldap.EntryAttribute{"accountStatus", []string{"inactive"}})
			} else {
				attrs = append(attrs, &ldap.EntryAttribute{"accountStatus", []string{"active"}})
			}

			if len(u.Mail) > 0 {
				attrs = append(attrs, &ldap.EntryAttribute{"mail", []string{u.Mail}})
			}

			attrs = append(attrs, &ldap.EntryAttribute{"objectClass", []string{"posixAccount"}})

			if len(u.LoginShell) > 0 {
				attrs = append(attrs, &ldap.EntryAttribute{"loginShell", []string{u.LoginShell}})
			} else {
				attrs = append(attrs, &ldap.EntryAttribute{"loginShell", []string{"/bin/bash"}})
			}

			if len(u.Homedir) > 0 {
				attrs = append(attrs, &ldap.EntryAttribute{"homeDirectory", []string{u.Homedir}})
			} else {
				attrs = append(attrs, &ldap.EntryAttribute{"homeDirectory", []string{"/home/" + u.Name}})
			}

			attrs = append(attrs, &ldap.EntryAttribute{"description", []string{fmt.Sprintf("%s via LDAP", u.Name)}})
			attrs = append(attrs, &ldap.EntryAttribute{"gecos", []string{fmt.Sprintf("%s via LDAP", u.Name)}})
			attrs = append(attrs, &ldap.EntryAttribute{"gidNumber", []string{fmt.Sprintf("%d", u.PrimaryGroup)}})
			attrs = append(attrs, &ldap.EntryAttribute{"memberOf", h.getGroupDNs(append(u.OtherGroups, u.PrimaryGroup))})
			if len(u.SSHKeys) > 0 {
				attrs = append(attrs, &ldap.EntryAttribute{"sshPublicKey", u.SSHKeys})
			}
			dn := fmt.Sprintf("cn=%s,ou=%s,%s", u.Name, h.getGroupName(u.PrimaryGroup), h.cfg.BaseDN)
			entries = append(entries, &ldap.Entry{dn, attrs})
		}
	}
	log.Printf("AP: Search OK: %s\n", searchReq.Filter)
	return ldap.ServerSearchResult{entries, []string{}, []ldap.Control{}, ldap.LDAPResultSuccess}, nil
}

//
func (h configHandler) Close(boundDn string, conn net.Conn) error {
	return nil
}

//
func (h configHandler) getGroupMembers(gid int) []string {
	members := make(map[string]bool)
	for _, u := range h.cfg.Users {
		if u.PrimaryGroup == gid {
			dn := fmt.Sprintf("cn=%s,ou=%s,%s", u.Name, h.getGroupName(u.PrimaryGroup), h.cfg.BaseDN)
			members[dn] = true
		} else {
			for _, othergid := range u.OtherGroups {
				if othergid == gid {
					dn := fmt.Sprintf("cn=%s,ou=%s,%s", u.Name, h.getGroupName(u.PrimaryGroup), h.cfg.BaseDN)
					members[dn] = true
				}
			}
		}
	}

	for _, g := range h.cfg.Groups {
		if gid == g.UnixID {
			for _, includegroupid := range g.IncludeGroups {
				if includegroupid != gid {
					includegroupmembers := h.getGroupMembers(includegroupid)

					for _, includegroupmember := range includegroupmembers {
						members[includegroupmember] = true
					}
				}
			}
		}
	}

	m := []string{}
	for k, _ := range members {
		m = append(m, k)
	}

	sort.Strings(m)

	return m
}

//
func (h configHandler) getGroupMemberIDs(gid int) []string {
	members := make(map[string]bool)
	for _, u := range h.cfg.Users {
		if u.PrimaryGroup == gid {
			members[u.Name] = true
		} else {
			for _, othergid := range u.OtherGroups {
				if othergid == gid {
					members[u.Name] = true
				}
			}
		}
	}

	for _, g := range h.cfg.Groups {
		if gid == g.UnixID {
			for _, includegroupid := range g.IncludeGroups {
				if includegroupid == gid {
					log.Printf("Group: %d - Ignoring myself as included group", includegroupid)
				} else {
					includegroupmemberids := h.getGroupMemberIDs(includegroupid)

					for _, includegroupmemberid := range includegroupmemberids {
						members[includegroupmemberid] = true
					}
				}
			}
		}
	}

	m := []string{}
	for k, _ := range members {
		m = append(m, k)
	}

	sort.Strings(m)

	return m
}

// Converts an array of GUIDs into an array of DNs
func (h configHandler) getGroupDNs(gids []int) []string {
	groups := make(map[string]bool)
	for _, gid := range gids {
		for _, g := range h.cfg.Groups {
			if g.UnixID == gid {
				dn := fmt.Sprintf("cn=%s,ou=groups,%s", g.Name, h.cfg.BaseDN)
				groups[dn] = true
			}

			for _, includegroupid := range g.IncludeGroups {
				if includegroupid == gid && g.UnixID != gid {
					includegroupdns := h.getGroupDNs([]int{g.UnixID})

					for _, includegroupdn := range includegroupdns {
						groups[includegroupdn] = true
					}
				}
			}
		}
	}

	g := []string{}
	for k, _ := range groups {
		g = append(g, k)
	}

	sort.Strings(g)

	return g
}

//
func (h configHandler) getGroupName(gid int) string {
	for _, g := range h.cfg.Groups {
		if g.UnixID == gid {
			return g.Name
		}
	}
	return ""
}
