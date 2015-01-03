// +build darwin,kqueue dragonfly freebsd netbsd openbsd
// +build !fsnotify

package notify

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
)

// TODO: Take into account currently monitored files with those read from dir.

// newWatcher returns `kqueue` Watcher implementation.
func newWatcher(c chan<- EventInfo) Watcher {
	k := &kqueue{
		idLkp:  make(map[int]*watched, 0),
		pthLkp: make(map[string]*watched, 0),
		c:      c,
		s:      make(chan struct{}),
	}
	if err := k.init(); err != nil {
		// TODO: Does it really has to be this way?
		panic(err)
	}
	go k.monitor()
	return k
}

// Close closes all still open file descriptors and kqueue.
func (k *kqueue) Close() error {
	k.Lock()
	if k.s != nil {
		close(k.s)
		k.s = nil
	}
	for _, w := range k.idLkp {
		syscall.Close(w.fd)
	}
	if k.fd != nil {
		syscall.Close(*k.fd)
		k.fd = nil
	}
	k.idLkp, k.pthLkp = nil, nil
	k.Unlock()
	return nil
}

// sendEvents sends reported events one by one through chan.
func (k *kqueue) sendEvents(evn []event) {
	for i := range evn {
		k.c <- &evn[i]
	}
}

// mask converts requested events to `kqueue` representation.
func mask(e Event) (o uint32) {
	o = uint32(e &^ Create)
	for k, n := range ekind {
		if e&n != 0 {
			o = (o &^ uint32(n)) | uint32(k)
		}
	}
	return
}

// unmask converts event received from `kqueue` to `notify.Event`
// representation taking into account requested events (`w`).
func unmask(o uint32, w Event) (e Event) {
	for k, n := range ekind {
		if o&uint32(k) != 0 {
			if w&k != 0 {
				e |= k
			}
			if w&n != 0 {
				e |= n
			}
		}
	}
	e |= Event(o) & w
	return
}

// del closes fd for `watched` and removes it from internal cache of monitored
// files/directories.
func (k *kqueue) del(w *watched) {
	syscall.Close(w.fd)
	delete(k.idLkp, w.fd)
	delete(k.pthLkp, w.p)
}

// monitor reads reported kqueue events and forwards them further after
// performing additional processing. If read event concerns directory,
// it generates Create/Delete event and sent them further instead of directory
// event. This event is detected based on reading contents of analyzed
// directory. If no changes in file list are detected, no event is send further.
// Reading directory structure is less accurate than kqueue and can lead
// to lack of detection of all events.
func (k *kqueue) monitor() {
	var evn []event
	// TODO: This is poor, and incorrect way to debug issue with events for
	// not registered fds, but temporarily for now might do. To be dropped.
	// TODO: Maybe len(kevn)>1 would help. What len in this case?
	hist := make(map[int]struct{})
	for {
		k.sendEvents(evn)
		evn = make([]event, 0)
		var kevn [1]syscall.Kevent_t
		select {
		case <-k.s:
			return
		default:
		}
		n, err := syscall.Kevent(*k.fd, nil, kevn[:], nil)
		if err != nil {
			fmt.Fprintf(os.Stderr, "kqueue: failed to read events: %q\n", err)
			continue
		}
		if n > 0 {
			k.Lock()
			w := k.idLkp[int(kevn[0].Ident)]
			if w == nil {
				if _, ok := hist[int(kevn[0].Ident)]; ok {
					fmt.Fprintf(os.Stderr, "kqueue: %v was previously registered\n", kevn[0])
				} else {
					fmt.Fprintf(os.Stderr, "kqueue: %v kevent is not registerd\n", kevn[0])
				}
				continue
			}
			hist[int(kevn[0].Ident)] = struct{}{}
			o := unmask(kevn[0].Fflags, w.eDir|w.eNonDir)
			if w.dir {
				// If it's dir and delete we have to send it and continue, because
				// other processing relies on opening (in this case not existing) dir.
				if (Event(kevn[0].Fflags) & NOTE_DELETE) != 0 {
					// Write is reported also for Delete on directory. Because of that
					// we have to filter it out explicitly.
					evn = append(evn, event{w.dir, w.p,
						o & ^Write & ^NOTE_WRITE, &kevn[0]})
					k.del(w)
					k.Unlock()
					continue
				}
				if (Event(kevn[0].Fflags) & NOTE_WRITE) != 0 {
					if err := k.walk(w.p, func(fi os.FileInfo) error {
						p := filepath.Join(w.p, fi.Name())
						if err := k.singlewatch(p, w.eDir, false, fi.IsDir()); err != nil {
							if err != errNoNewWatch {
								// TODO: pass error via chan because state of monitoring is
								// invalid.
								panic(err)
							}
						} else if (w.eDir & Create) != 0 {
							evn = append(evn, event{fi.IsDir(), p, Create, nil})
						}
						return nil
					}); err != nil {
						// TODO: pass error via chan because state of monitoring is invalid.
						panic(err)
					}
				}
			} else {
				evn = append(evn, event{w.dir, w.p, o, &kevn[0]})
			}
			if (Event(kevn[0].Fflags) & NOTE_DELETE) != 0 {
				k.del(w)
			}
			k.Unlock()
		}
	}
}

// kqueue is a type holding data for kqueue watcher.
type kqueue struct {
	sync.Mutex
	// fd is a kqueue file descriptor
	fd *int
	// idLkp is a data structure mapping file descriptors with data about watching
	// represented by them files/directories.
	idLkp map[int]*watched
	// pthLkp is a data structure mapping file names with data about watching
	// represented by them files/directories.
	pthLkp map[string]*watched
	// c is a channel used to pass events further.
	c chan<- EventInfo
	// s is a channel used to stop monitoring.
	s chan struct{}
}

// watched is a data structure representing watched file/directory.
type watched struct {
	// p is a path to watched file/directory.
	p string
	// fd is a file descriptor for watched file/directory.
	fd int
	// dir is a boolean specifying if watched is directory.
	dir bool
	// eDir represents events watched directly.
	eDir Event
	// eNonDir represents events watched indirectly.
	eNonDir Event
}

// init initializes kqueue.
func (k *kqueue) init() error {
	fd, err := syscall.Kqueue()
	if err != nil {
		return err
	}
	k.fd = &fd
	return nil
}

func (k *kqueue) watch(p string, e Event, isdir bool) error {
	if err := k.singlewatch(p, e, true, isdir); err != nil {
		if err != errNoNewWatch {
			return nil
		}
	}
	if isdir {
		err := k.walk(p, func(fi os.FileInfo) (err error) {
			if err = k.singlewatch(filepath.Join(p, fi.Name()),
				e, false, fi.IsDir()); err != nil {
				if err != errNoNewWatch {
					return
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}

var errNoNewWatch = errors.New("kqueue: file already watched")
var errNotWatched = errors.New("kqueue: cannot unwatch not watched file")

// watch starts to watch given `p` file/directory.
func (k *kqueue) singlewatch(p string, e Event, direct, dir bool) error {
	w, ok := k.pthLkp[p]
	if !ok {
		fd, err := syscall.Open(p, syscall.O_NONBLOCK|syscall.O_RDONLY, 0)
		if err != nil {
			return err
		}
		w = &watched{fd: fd, p: p, dir: dir}
	}
	if direct {
		w.eDir |= e
	} else {
		w.eNonDir |= e
	}
	var kevn [1]syscall.Kevent_t
	syscall.SetKevent(&kevn[0], w.fd, syscall.EVFILT_VNODE, syscall.EV_ADD|syscall.EV_CLEAR)
	kevn[0].Fflags = mask(w.eDir | w.eNonDir)
	if _, err := syscall.Kevent(*k.fd, kevn[:], nil, nil); err != nil {
		return err
	}
	if !ok {
		k.idLkp[w.fd], k.pthLkp[w.p] = w, w
		return nil
	}
	return errNoNewWatch
}

// unwatch stops watching `p` file/directory.
func (k *kqueue) singleunwatch(p string, direct bool) error {
	w := k.pthLkp[p]
	if w == nil {
		return errNotWatched
	}
	if direct {
		w.eDir = 0
	} else {
		w.eNonDir = 0
	}
	var kevn [1]syscall.Kevent_t
	syscall.SetKevent(&kevn[0], w.fd, syscall.EVFILT_VNODE, syscall.EV_DELETE)
	if _, err := syscall.Kevent(*k.fd, kevn[:], nil, nil); err != nil {
		return err
	}
	if w.eNonDir&w.eDir != 0 {
		if err := k.singlewatch(p, w.eNonDir|w.eDir, w.eNonDir == 0, w.dir); err != nil {
			return err
		}
	} else {
		k.del(w)
	}
	return nil
}

// walk runs `f` func on each file from `p` directory.
func (k *kqueue) walk(p string, f func(os.FileInfo) error) error {
	fp, err := os.Open(p)
	if err != nil {
		return err
	}
	ls, err := fp.Readdir(0)
	fp.Close()
	if err != nil {
		return err
	}
	for i := range ls {
		if err := f(ls[i]); err != nil {
			return err
		}
	}
	return nil
}

func (k *kqueue) unwatch(p string, isdir bool) error {
	if isdir {
		err := k.walk(p, func(fi os.FileInfo) error {
			if !fi.IsDir() {
				return k.singleunwatch(filepath.Join(p, fi.Name()), false)
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	return k.singleunwatch(p, true)
}

// Watch implements Watcher interface.
func (k *kqueue) Watch(p string, e Event) (err error) {
	b, err := isdir(p)
	if err != nil {
		return
	}
	k.Lock()
	err = k.watch(p, e, b)
	k.Unlock()
	return
}

// Unwatch implements Watcher interface.
func (k *kqueue) Unwatch(p string) (err error) {
	b, err := isdir(p)
	if err != nil {
		return
	}
	k.Lock()
	err = k.unwatch(p, b)
	k.Unlock()
	return
}

// Rewatch implements Watcher interface.
//
// TODO(rjeczalik): This is a naive hack. Rewrite might help.
func (k *kqueue) Rewatch(p string, _, e Event) (err error) {
	b, err := isdir(p)
	if err != nil {
		return
	}
	k.Lock()
	if err = k.unwatch(p, b); err == nil {
		// TODO(rjeczalik): If watch fails then we leave kqueue in inconsistent
		// state. Handle? Panic? Native version of rewatch?
		err = k.watch(p, e, b)
	}
	k.Unlock()
	return
}

// isdir returns a boolean indicating if `p` string represents
// path to a directory.
func isdir(p string) (bool, error) {
	fi, err := os.Stat(p)
	if err != nil {
		return false, err
	}
	return fi.IsDir(), nil
}
