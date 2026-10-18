package stellar

import (
	"testing"

	"github.com/stellar/go-stellar-sdk/keypair"
	"github.com/stellar/go-stellar-sdk/txnbuild"
	"github.com/stellar/go-stellar-sdk/xdr"
)

// benchWasmSize approximates a small real contract module.
const benchWasmSize = 50 * 1024

// benchSimulation encodes the SorobanTransactionData a simulation would
// return. It mirrors simulatedData in assemble_test.go, which takes a
// *testing.T and so cannot be called from a benchmark.
func benchSimulation(b *testing.B) SimulateResult {
	b.Helper()

	const resourceFee = 87_654
	data := xdr.SorobanTransactionData{
		Resources: xdr.SorobanResources{
			Instructions:  xdr.Uint32(1_000_000),
			DiskReadBytes: xdr.Uint32(500),
			WriteBytes:    xdr.Uint32(1_200),
		},
		ResourceFee: xdr.Int64(resourceFee),
	}
	encoded, err := xdr.MarshalBase64(data)
	if err != nil {
		b.Fatal(err)
	}
	return SimulateResult{TransactionDataXDR: encoded, MinResourceFee: resourceFee}
}

func benchBuildParams(b *testing.B) BuildParams {
	b.Helper()

	address := keypair.MustRandom().Address()
	return BuildParams{
		Source:    &txnbuild.SimpleAccount{AccountID: address, Sequence: 41},
		Operation: UploadWasmOp(make([]byte, benchWasmSize), address),
		BaseFee:   txnbuild.MinBaseFee,
	}
}

func BenchmarkBuild(b *testing.B) {
	params := benchBuildParams(b)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// Build consumes a sequence number from the source on each call, which
		// is the same work every real deploy does.
		if _, err := Build(params); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkAssemble(b *testing.B) {
	tx, err := Build(benchBuildParams(b))
	if err != nil {
		b.Fatal(err)
	}
	sim := benchSimulation(b)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := Assemble(tx, sim); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkUploadWasmOp(b *testing.B) {
	wasm := make([]byte, benchWasmSize)
	source := keypair.MustRandom().Address()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if UploadWasmOp(wasm, source) == nil {
			b.Fatal("nil operation")
		}
	}
}
