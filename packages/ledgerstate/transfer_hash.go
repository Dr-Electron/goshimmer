package ledgerstate

import (
	"unicode/utf8"

	"github.com/iotaledger/goshimmer/packages/stringify"
)

type TransferHash [transferHashLength]byte

func NewTransferHash(transferHash string) (result TransferHash) {
	copy(result[:], transferHash)

	return
}

func (transferHash TransferHash) ToRealityId() (realityId RealityId) {
	copy(realityId[:], transferHash[:])

	return
}

func (transferHash *TransferHash) UnmarshalBinary(data []byte) error {
	copy(transferHash[:], data[:transferHashLength])

	return nil
}

func (transferHash TransferHash) String() string {
	if utf8.Valid(transferHash[:]) {
		return string(transferHash[:])
	} else {
		return stringify.SliceOfBytes(transferHash[:])
	}
}

const transferHashLength = 32