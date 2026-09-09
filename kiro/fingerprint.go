// Derived from CLIProxyAPIPlus commit 1fec8453e63a5bc133555a79164480700e351bfc; MIT licensed.

package main

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math/rand"
	"runtime"
)

type Fingerprint struct {
	OIDCSDKVersion, RuntimeSDKVersion, StreamingSDKVersion string
	OSType, OSVersion, NodeVersion, KiroVersion, KiroHash  string
}

var (
	oidcSDKVersions = []string{"3.980.0", "3.975.0", "3.972.0", "3.808.0", "3.738.0", "3.737.0", "3.736.0", "3.735.0"}
	osVersions      = map[string][]string{"darwin": {"25.2.0", "25.1.0", "25.0.0", "24.5.0"}, "windows": {"10.0.26200", "10.0.26100", "10.0.22631"}, "linux": {"6.12.0", "6.11.0", "6.8.0", "6.6.0", "6.5.0", "6.1.0"}}
	nodeVersions    = []string{"22.21.1", "22.21.0", "22.20.0", "22.19.0", "22.18.0", "20.18.0", "20.17.0", "20.16.0"}
	kiroVersions    = []string{"0.10.32", "0.10.16", "0.10.10", "0.9.47", "0.9.40", "0.9.2", "0.8.206", "0.8.140", "0.8.135", "0.8.86"}
)

func accountKey(seed string) string {
	sum := sha256.Sum256([]byte(seed))
	return hex.EncodeToString(sum[:8])
}

func fingerprint(seed string) Fingerprint {
	sum := sha256.Sum256([]byte(seed))
	rng := rand.New(rand.NewSource(int64(binary.BigEndian.Uint64(sum[:8]))))
	osType := runtime.GOOS
	versions, ok := osVersions[osType]
	if !ok {
		osType = "linux"
		versions = osVersions[osType]
	}
	return Fingerprint{OIDCSDKVersion: oidcSDKVersions[rng.Intn(len(oidcSDKVersions))], RuntimeSDKVersion: "1.0.0", StreamingSDKVersion: "1.0.27", OSType: osType, OSVersion: versions[rng.Intn(len(versions))], NodeVersion: nodeVersions[rng.Intn(len(nodeVersions))], KiroVersion: kiroVersions[rng.Intn(len(kiroVersions))], KiroHash: hex.EncodeToString(sum[:])}
}

func (fp Fingerprint) UserAgent() string {
	return fmt.Sprintf("aws-sdk-js/%s ua/2.1 os/%s#%s lang/js md/nodejs#%s api/codewhispererstreaming#%s m/E KiroIDE-%s-%s", fp.StreamingSDKVersion, fp.OSType, fp.OSVersion, fp.NodeVersion, fp.StreamingSDKVersion, fp.KiroVersion, fp.KiroHash)
}
func (fp Fingerprint) AmzUserAgent() string {
	return fmt.Sprintf("aws-sdk-js/%s KiroIDE-%s-%s", fp.StreamingSDKVersion, fp.KiroVersion, fp.KiroHash)
}
