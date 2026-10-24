package machine_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/discobox-ai/vm/pkg/machine"
	_ "github.com/discobox-ai/vm/pkg/machine/drivers"
)

func TestAKeyPrintsWithoutItsSecret(t *testing.T) {
	key := machine.Key{Driver: "boxd", Secret: "bxd_very-secret", ID: "key_1", Name: "pool", Issued: true}
	for _, out := range []string{fmt.Sprint(key), fmt.Sprintf("%v %+v %#v %s %q", key, key, key, key, key), key.LogValue().String()} {
		if strings.Contains(out, key.Secret) {
			t.Fatalf("printed %s", out)
		}
	}
}

func TestKeyFromEnvAndWithoutKeys(t *testing.T) {
	t.Setenv("BOXD_API_KEY", "")
	t.Setenv("BOXD_TOKEN", "from-token")
	key, err := machine.KeyFromEnv("boxd")(t.Context())
	if err != nil || key.Secret != "from-token" || key.Driver != "boxd" {
		t.Fatalf("boxd's key from the environment: %v, %v", key, err)
	}
	if machine.KeyFromEnv("docker") != nil {
		t.Fatal("docker has a key in the environment")
	}
	env := machine.WithoutKeys([]string{"PATH=/bin", "BOXD_TOKEN=x", "BOXD_API_KEY=y", "DISCO_VM_FAKE_KEY=z", "HOME=/h"})
	if !slices.Equal(env, []string{"PATH=/bin", "HOME=/h"}) {
		t.Fatalf("without keys: %v", env)
	}
}
