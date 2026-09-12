package mgr

import (
	"errors"
	"regexp"
	"strconv"
	"strings"

	"vpsmgr/internal/db"
)

var childNameRe = regexp.MustCompile(`^([a-z][a-z0-9-]*[a-z])-([0-9]+)$`)
var numericSuffixRe = regexp.MustCompile(`-[0-9]+$`)

func UserGroupLabel(name string) string {
	g := ParseUserGroup(name)
	if !g.Child {
		return "A"
	}
	return name[len(g.Parent)+1:]
}

// MachineSpecs renders a compact quota tag for the container switcher:
// "<cpu>c <mem>g <disk>g", e.g. "4c 8g 40g". CPU reuses FormatCPU (tenths of a
// core, so 0.5 stays fractional); memory is MiB rendered as GiB with a trimmed
// decimal (1024 -> "1", 512 -> "0.5"); disk is already GiB.
func MachineSpecs(cpuTenths, memMB, diskGB int) string {
	mem := strconv.FormatFloat(float64(memMB)/1024, 'f', -1, 64)
	return FormatCPU(cpuTenths) + "c " + mem + "g " + strconv.Itoa(diskGB) + "g"
}

type UserGroup struct {
	Name   string
	Parent string
	Child  bool
}

func ParseUserGroup(name string) UserGroup {
	name = strings.ToLower(name)
	m := childNameRe.FindStringSubmatch(name)
	if len(m) == 0 || childNameRe.MatchString(m[1]) {
		return UserGroup{Name: name}
	}
	return UserGroup{Name: name, Parent: m[1], Child: true}
}

func UserGroupName(name string) string {
	g := ParseUserGroup(name)
	if g.Child {
		return g.Parent
	}
	return g.Name
}

func (m *Manager) ValidateAddName(name string, allowChild bool) error {
	name = strings.ToLower(name)
	g := ParseUserGroup(name)
	if !g.Child {
		if numericSuffixRe.MatchString(name) {
			return errors.New("invalid child username: text before -number must start and end with a letter")
		}
		return ValidateName(name)
	}
	if !allowChild {
		return errors.New("child users can only be created from the admin panel")
	}
	if len(name) > 31 {
		return errors.New("invalid child username: max 31 characters")
	}
	return nil
}

func (m *Manager) UsersInGroup(name string) ([]*db.User, error) {
	group := UserGroupName(name)
	users, err := m.db.ListUsers()
	if err != nil {
		return nil, err
	}
	out := make([]*db.User, 0)
	for _, u := range users {
		if UserGroupName(u.Name) == group {
			out = append(out, u)
		}
	}
	return out, nil
}

func (m *Manager) SetGroupColor(name, color string) error {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	users, err := m.UsersInGroup(name)
	if err != nil {
		return err
	}
	ids := make([]int64, 0, len(users))
	for _, u := range users {
		ids = append(ids, u.ID)
	}
	return m.db.UpdateUsersColor(ids, color)
}

func (m *Manager) inheritGroupColorLocked(name string, excludeID int64) error {
	users, err := m.UsersInGroup(name)
	if err != nil {
		return err
	}
	for _, u := range users {
		if u.ID != excludeID && u.Color != "" {
			return m.db.UpdateUsersColor([]int64{excludeID}, u.Color)
		}
	}
	return nil
}
