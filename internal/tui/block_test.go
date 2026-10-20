package tui

import (
	"strings"
	"time"

	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/proto"
)

// blockModel is just enough of a Model to draw message blocks: alice (1)
// is me; bob, carol (hostile name) and a 40-x name are others.
func blockModel(color bool) *Model {
	r := testRenderer(color)
	users := map[uint64]proto.User{1: {ID: 1, Name: "alice"}, 2: {ID: 2, Name: "bob"}, 3: {ID: 3, Name: "carol\x1b[2J"},
		4: {ID: 4, Name: strings.Repeat("x", 40)}, 9: {ID: 9, Name: "ci", Bot: true}}
	m := &Model{st: r.st, r: *r, me: users[1], users: users, now: time.Now}
	m.fx.init()
	m.fx.off = true
	m.mentionAs = []string{"alice"}
	return m
}

// block draws x at width w and returns it with its rows as plain text.
func block(m *Model, x client.Message, w int, header bool) (*canvas, []string) {
	b := m.msgBlock(x, w, header, true, false)
	rows := make([]string, b.h)
	for y := range rows {
		rows[y] = prow{b: b, y: y}.text()
	}
	return b, rows
}

// rowWith is the index of the first row containing s, or -1.
func rowWith(rows []string, s string) int {
	for i, r := range rows {
		if strings.Contains(r, s) {
			return i
		}
	}
	return -1
}

// cellAt finds s in row y and returns its first cell.
func cellAt(b *canvas, rows []string, y int, s string) *cell {
	if y < 0 {
		return nil
	}
	i := strings.Index(rows[y], s)
	if i < 0 {
		return nil
	}
	return b.at(strWidth(rows[y][:i]), y)
}
