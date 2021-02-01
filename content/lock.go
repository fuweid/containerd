/*
   Copyright The containerd Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package content

import (
	"context"
	"sync"

	"golang.org/x/sync/semaphore"
)

type keyWeighted struct {
	waiter *semaphore.Weighted
	cnt    int
}

type trylock struct {
	sync.Mutex

	locks map[string]*keyWeighted
}

func (l *trylock) lock(ctx context.Context, key string) error {
	l.Lock()
	w, ok := l.locks[key]
	if !ok {
		l.locks[key] = &keyWeighted{
			waiter: semaphore.NewWeighted(1),
		}
		w = l.locks[key]
	}
	w.cnt++
	l.Unlock()

	if err := w.waiter.Acquire(ctx, 1); err != nil {
		l.Lock()
		w.cnt--
		if w.cnt <= 0 {
			delete(l.locks, key)
		}
		l.Unlock()
		return err
	}
	return nil
}

func (l *trylock) unlock(key string) {
	l.Lock()
	w, ok := l.locks[key]
	if ok {
		w.waiter.Release(1)
		w.cnt--
		if w.cnt <= 0 {
			delete(l.locks, key)
		}
	}
	l.Unlock()
}
