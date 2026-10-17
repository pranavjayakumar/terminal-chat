package tui

import (
	"strconv"

	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/proto"
)

// NotifyBackend for the fake: the same parsing as the client, in memory.

func (f *fakeClient) NotifyPrefs() client.NotifyPrefs {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := client.NotifyPrefs{Chans: map[uint64]client.ChanNotify{}, CueOff: map[string]bool{}, Volume: client.DefaultCueVolume}
	for ch, c := range f.nchans {
		p.Chans[ch] = c
	}
	if l, err := client.ParseLevel(f.nset["notify.default"]); err == nil {
		p.Default = l
	}
	p.Content = f.nset["notify.content"]
	p.Quiet = f.nset["notify.quiet"]
	p.DNDUntil, _ = strconv.ParseInt(f.nset["notify.dnd_until"], 10, 64)
	p.SoundOff = f.nset["sound.all"] == "off"
	for _, c := range client.SoundCues {
		p.CueOff[c] = f.nset["sound."+c] == "off"
	}
	if v, err := strconv.Atoi(f.nset["sound.volume"]); err == nil {
		p.Volume = v
	}
	return p
}

func (f *fakeClient) SetChanNotify(ch uint64, lvl client.NotifyLevel, until int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.nchans == nil {
		f.nchans = map[uint64]client.ChanNotify{}
	}
	if lvl == client.LevelDefault {
		delete(f.nchans, ch)
	} else {
		f.nchans[ch] = client.ChanNotify{Level: lvl, Until: until}
	}
	f.calls = append(f.calls, "notify "+strconv.FormatUint(ch, 10)+" "+string(lvl))
	return nil
}

func (f *fakeClient) SetNotifySetting(key, value string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.nset == nil {
		f.nset = map[string]string{}
	}
	if value == "" {
		delete(f.nset, key)
	} else {
		f.nset[key] = value
	}
	f.calls = append(f.calls, "set "+key+"="+value)
	return nil
}

func (f *fakeClient) InMyThread(m proto.Message) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.myThreads[m.Thread]
}

