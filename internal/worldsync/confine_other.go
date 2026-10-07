//go:build !linux

/*
Copyright paul_wtf.

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

package worldsync

import (
	"errors"
	"io"
	"os"

	"golang.org/x/sys/unix"

	"github.com/spawnery/spawnery/internal/prune"
)

func openDir(string, []string, bool, int) (int, error) { return -1, errors.ErrUnsupported }

func openRegular(string, string) (*os.File, error) { return nil, errors.ErrUnsupported }

func lstatAt(string, string) error { return errors.ErrUnsupported }

func place(string, string, io.Reader, uint32, int64, int) error { return errors.ErrUnsupported }

func walkKept(string, prune.Keep, func(int, string, string, *unix.Stat_t) error) error {
	return errors.ErrUnsupported
}

func regroupKept(string, prune.Keep, int) error { return errors.ErrUnsupported }

func worldOutside(string, prune.Keep, prune.Keep) (string, error) { return "", errors.ErrUnsupported }

func entryHoldsWorld(string, string) (bool, error) { return false, errors.ErrUnsupported }
