// Copyright 2016 Keybase Inc. All rights reserved.
// Use of this source code is governed by a BSD
// license that can be found in the LICENSE file.

package libkbfs

import (
	"github.com/keybase/kbfs/kbfscodec"
	"github.com/keybase/kbfs/kbfscrypto"
	"github.com/keybase/kbfs/tlf"
)

// FakeInitialRekey fakes the initial rekey for the given
// BareRootMetadata. This is necessary since newly-created
// BareRootMetadata objects don't have enough data to build a
// TlfHandle from until the first rekey. pubKey is non-empty only for
// server-side tests.
func FakeInitialRekey(md MutableBareRootMetadata, codec kbfscodec.Codec,
	crypto cryptoPure, h tlf.Handle,
	pubKey kbfscrypto.TLFPublicKey) ExtraMetadata {
	var readerEPubKeyIndex int
	// Apply the "negative hack" for V2 and earlier.
	if md.Version() <= InitialExtraMetadataVer {
		readerEPubKeyIndex = -1
	}
	wDkim := make(UserDeviceKeyInfoMap)
	for _, w := range h.Writers {
		k := kbfscrypto.MakeFakeCryptPublicKeyOrBust(string(w))
		wDkim[w] = DeviceKeyInfoMap{
			k: TLFCryptKeyInfo{},
		}
	}

	rDkim := make(UserDeviceKeyInfoMap)
	for _, r := range h.Readers {
		k := kbfscrypto.MakeFakeCryptPublicKeyOrBust(string(r))
		rDkim[r] = DeviceKeyInfoMap{
			k: TLFCryptKeyInfo{
				EPubKeyIndex: readerEPubKeyIndex,
			},
		}
	}

	tlfCryptKey := kbfscrypto.MakeTLFCryptKey([32]byte{0x1})
	return md.addKeyGenerationForTest(codec, crypto, nil,
		kbfscrypto.TLFCryptKey{}, tlfCryptKey, pubKey, wDkim, rDkim)
}
