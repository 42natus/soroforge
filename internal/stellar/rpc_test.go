package stellar

import (
	"testing"

	"github.com/stellar/go-stellar-sdk/xdr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFeeChargedFromResultXDR(t *testing.T) {
	results := []xdr.OperationResult{}
	encoded, err := xdr.MarshalBase64(xdr.TransactionResult{
		FeeCharged: xdr.Int64(7654321),
		Result: xdr.TransactionResultResult{
			Code:    xdr.TransactionResultCodeTxSuccess,
			Results: &results,
		},
		Ext: xdr.TransactionResultExt{V: 0},
	})
	require.NoError(t, err)

	fee := feeChargedFromResultXDR(encoded)
	require.NotNil(t, fee)
	assert.Equal(t, int64(7654321), *fee)
	assert.Nil(t, feeChargedFromResultXDR("not-xdr"))
	assert.Nil(t, feeChargedFromResultXDR(""))
}
