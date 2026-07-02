/*
   Copyright 2020 Docker Compose CLI authors

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

package errdefs

import (
	"github.com/containerd/errdefs"
)

func InvalidParameter(err error) error {
	return errdefs.ErrInvalidArgument.WithMessage(err.Error())
}

func NotFound(err error) error {
	return errdefs.ErrNotFound.WithMessage(err.Error())
}

func Conflict(err error) error {
	return errdefs.ErrConflict.WithMessage(err.Error())
}

func IsInvalidParameter(err error) bool {
	return errdefs.IsInvalidArgument(err)
}

func IsNotFound(err error) bool {
	return errdefs.IsNotFound(err)
}

func IsConflict(err error) bool {
	return errdefs.IsConflict(err)
}

func IsUnauthorized(err error) bool {
	return errdefs.IsUnauthorized(err)
}
