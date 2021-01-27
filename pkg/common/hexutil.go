/*
Copyright © 2020 FairOS Authors

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

package common

import (
	"encoding/hex"
	"hash"
	"strconv"

	"github.com/ethersphere/bee/pkg/swarm"
	bmtlegacy "github.com/ethersphere/bmt/legacy"
	"golang.org/x/crypto/sha3"
)

type decError struct{ msg string }

func (err decError) Error() string { return err.msg }

var (
	ErrEmptyString   = &decError{"empty hex string"}
	ErrMissingPrefix = &decError{"hex string without 0x prefix"}
	ErrSyntax        = &decError{"invalid hex string"}
	ErrOddLength     = &decError{"hex string of odd length"}
	ErrUint64Range   = &decError{"hex number > 64 bits"}
)

// Encode encodes b as a hex string with 0x prefix.
func Encode(b []byte) string {
	enc := make([]byte, len(b)*2)
	hex.Encode(enc, b)
	return string(enc)
}

// Decode decodes a hex string with 0x prefix.
func Decode(input string) ([]byte, error) {
	if input == "" {
		return nil, ErrEmptyString
	}
	if !has0xPrefix(input) {
		return nil, ErrMissingPrefix
	}
	b, err := hex.DecodeString(input[2:])
	if err != nil {
		err = mapError(err)
	}
	return b, err
}

func has0xPrefix(input string) bool {
	return len(input) >= 2 && input[0] == '0' && (input[1] == 'x' || input[1] == 'X')
}

func mapError(err error) error {
	if err, ok := err.(*strconv.NumError); ok {
		switch err.Err {
		case strconv.ErrRange:
			return ErrUint64Range
		case strconv.ErrSyntax:
			return ErrSyntax
		}
	}
	if _, ok := err.(hex.InvalidByteError); ok {
		return ErrSyntax
	}
	if err == hex.ErrLength {
		return ErrOddLength
	}
	return err
}

func hashFunc() hash.Hash {
	return sha3.NewLegacyKeccak256()
}

func HashString(path string) []byte {
	p := bmtlegacy.NewTreePool(hashFunc, swarm.Branches, bmtlegacy.PoolSize)
	hasher := bmtlegacy.New(p)
	hasher.Reset()
	_, err := hasher.Write([]byte(path))
	if err != nil {
		return []byte{0}
	}
	return hasher.Sum(nil)
}
