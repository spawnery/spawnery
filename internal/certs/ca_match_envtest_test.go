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

package certs

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

const unparsableBlock = "-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n"

// seedSecret fills in tls.crt and tls.key if data has none: a TLS secret
// cannot be created without the keys.
func seedSecret(t *testing.T, ctx context.Context, s *Store, data map[string][]byte) {
	t.Helper()
	for _, key := range []string{"tls.crt", "tls.key"} {
		if _, ok := data[key]; !ok {
			data[key] = []byte("x")
		}
	}
	err := s.Client.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: s.Name, Namespace: s.Namespace},
		Type:       corev1.SecretTypeTLS,
		Data:       data,
	})
	if err != nil {
		t.Fatalf("create the secret: %v", err)
	}
}

func readSecret(t *testing.T, ctx context.Context, s *Store) *corev1.Secret {
	t.Helper()
	secret := &corev1.Secret{}
	if err := s.Client.Get(ctx, types.NamespacedName{Name: s.Name, Namespace: s.Namespace}, secret); err != nil {
		t.Fatalf("get the secret: %v", err)
	}
	return secret
}

func issued(t *testing.T, s *Store) *Bundle {
	t.Helper()
	b, err := Issue(s.Clock(), s.DNSNames)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func dataOf(b *Bundle, caCert []byte) map[string][]byte {
	return map[string][]byte{
		"ca.crt": caCert, "ca.key": b.CAKeyPEM, "tls.crt": b.ServingCertPEM, "tls.key": b.ServingKeyPEM,
	}
}

func TestTheCertificateThatMatchesTheKeyIsFoundBehindAnotherOne(t *testing.T) {
	for name, front := range map[string]func(*testing.T, *Store) []byte{
		"another CA":                  func(t *testing.T, s *Store) []byte { return issued(t, s).CACertPEM },
		"a block that does not parse": func(*testing.T, *Store) []byte { return []byte(unparsableBlock) },
	} {
		t.Run(name, func(t *testing.T) {
			s, rec, ctx, _, now := newRecordingStore(t)
			real := issued(t, s)
			seedSecret(t, ctx, s, dataOf(real, slices.Concat(front(t, s), real.CACertPEM)))

			got, err := s.Ensure(ctx)
			if err != nil {
				t.Fatalf("Ensure: %v", err)
			}
			if !bytes.Equal(got.CACertPEM, real.CACertPEM) {
				t.Fatal("Ensure did not keep the CA that ca.key belongs to")
			}
			if err := got.Validate(now, s.DNSNames); err != nil {
				t.Errorf("the repaired bundle is invalid: %v", err)
			}
			stored := readSecret(t, ctx, s)
			if !bytes.Equal(stored.Data["ca.crt"], real.CACertPEM) {
				t.Errorf("ca.crt was not repaired to the matching certificate alone:\n%s", stored.Data["ca.crt"])
			}
			if !bytes.Equal(stored.Data["ca.key"], real.CAKeyPEM) {
				t.Error("ca.key changed")
			}
			record := stored.Annotations[AnnotationRotationDiscarded]
			if !strings.Contains(record, "ca.crt") || !strings.Contains(record, "certificate 2 of 2") || !strings.Contains(record, now.Format(time.RFC3339)) {
				t.Errorf("the record %q does not name the slot, the block and the time", record)
			}
			expectEvent(t, rec, corev1.EventTypeWarning, ReasonCACertificateRepaired)
			if got := testutil.ToFloat64(CAKeyUnmatched); got != 0 {
				t.Errorf("spawnery_ca_key_unmatched = %v after a repair", got)
			}
		})
	}
}

func TestARepairedCAConvergesAndKeepsItsRotationSlots(t *testing.T) {
	s, _, ctx, _, _ := newRecordingStore(t)
	real, next := issued(t, s), issued(t, s)
	data := dataOf(real, slices.Concat(issued(t, s).CACertPEM, real.CACertPEM))
	data["ca-next.crt"], data["ca-next.key"] = next.CACertPEM, next.CAKeyPEM
	seedSecret(t, ctx, s, data)

	first, err := s.Ensure(ctx)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.Ensure(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first.CACertPEM, real.CACertPEM) {
		t.Error("the repair did not keep the CA ca.key belongs to")
	}
	if !bytes.Equal(first.CACertPEM, second.CACertPEM) || !bytes.Equal(first.ServingCertPEM, second.ServingCertPEM) {
		t.Error("the repair did not converge")
	}
	if !bytes.Equal(second.NextCACertPEM, next.CACertPEM) {
		t.Error("the repair dropped the incoming CA")
	}
}

func TestACAKeyWithNoMatchingCertificateIsNotReplaced(t *testing.T) {
	cases := map[string]func(t *testing.T, s *Store) map[string][]byte{
		"ca.crt holds another CA": func(t *testing.T, s *Store) map[string][]byte {
			real := issued(t, s)
			return dataOf(real, issued(t, s).CACertPEM)
		},
		"ca.crt holds another CA and a block that does not parse": func(t *testing.T, s *Store) map[string][]byte {
			real := issued(t, s)
			return dataOf(real, slices.Concat([]byte(unparsableBlock), issued(t, s).CACertPEM))
		},
		"ca.crt is empty": func(t *testing.T, s *Store) map[string][]byte {
			return dataOf(issued(t, s), nil)
		},
		"ca.crt is not PEM": func(t *testing.T, s *Store) map[string][]byte {
			return dataOf(issued(t, s), []byte("kaputt"))
		},
		"ca.key is not a key": func(t *testing.T, s *Store) map[string][]byte {
			real := issued(t, s)
			d := dataOf(real, real.CACertPEM)
			d["ca.key"] = []byte("kaputt")
			return d
		},
	}
	for name, build := range cases {
		t.Run(name, func(t *testing.T) {
			s, rec, ctx, _, _ := newRecordingStore(t)
			seedSecret(t, ctx, s, build(t, s))
			before := readSecret(t, ctx, s)

			got, err := s.Ensure(ctx)
			if !errors.Is(err, ErrCAKeyUnmatched) {
				t.Fatalf("Ensure = %v, %v; want ErrCAKeyUnmatched", got, err)
			}
			after := readSecret(t, ctx, s)
			if after.ResourceVersion != before.ResourceVersion {
				t.Error("the refusal wrote to the secret")
			}
			for key, want := range before.Data {
				if !bytes.Equal(after.Data[key], want) {
					t.Errorf("%s changed", key)
				}
			}
			got1 := expectEvent(t, rec, corev1.EventTypeWarning, ReasonCAReissueRefused)
			if !strings.Contains(got1, "no new CA is issued") {
				t.Errorf("event = %q, want it to say what the operator did not do", got1)
			}
			if v := testutil.ToFloat64(CAKeyUnmatched); v != 1 {
				t.Errorf("spawnery_ca_key_unmatched = %v, want 1", v)
			}
		})
	}
}

func TestTheRefusalEndsWhenTheSecretIsRepaired(t *testing.T) {
	s, _, ctx, _, _ := newRecordingStore(t)
	real := issued(t, s)
	seedSecret(t, ctx, s, dataOf(real, issued(t, s).CACertPEM))
	if _, err := s.Ensure(ctx); !errors.Is(err, ErrCAKeyUnmatched) {
		t.Fatalf("Ensure = %v, want ErrCAKeyUnmatched", err)
	}

	secret := readSecret(t, ctx, s)
	secret.Data["ca.crt"] = real.CACertPEM
	if err := s.Client.Update(ctx, secret); err != nil {
		t.Fatal(err)
	}
	got, err := s.Ensure(ctx)
	if err != nil {
		t.Fatalf("Ensure after the repair: %v", err)
	}
	if !bytes.Equal(got.CACertPEM, real.CACertPEM) {
		t.Error("the CA changed")
	}
	if v := testutil.ToFloat64(CAKeyUnmatched); v != 0 {
		t.Errorf("spawnery_ca_key_unmatched = %v after the repair, want 0", v)
	}
}

func TestASecretWithNoCAKeyGetsANewCA(t *testing.T) {
	for name, data := range map[string]map[string][]byte{
		"no data":           {},
		"only a ca.crt":     {"ca.crt": issued(t, &Store{Clock: time.Now, DNSNames: testDNSNames()}).CACertPEM},
		"an empty ca.key":   {"ca.crt": []byte("x"), "ca.key": {}},
		"a blank ca.key":    {"ca.key": []byte("\n")},
		"only serving pair": {"tls.crt": []byte("x"), "tls.key": []byte("y")},
	} {
		t.Run(name, func(t *testing.T) {
			s, _, ctx, _, now := newRecordingStore(t)
			seedSecret(t, ctx, s, data)

			got, err := s.Ensure(ctx)
			if err != nil {
				t.Fatalf("Ensure: %v", err)
			}
			if err := got.Validate(now, s.DNSNames); err != nil {
				t.Errorf("the new bundle is invalid: %v", err)
			}
			if stored := readSecret(t, ctx, s); !bytes.Equal(stored.Data["ca.crt"], got.CACertPEM) {
				t.Error("the new CA was not stored")
			}
		})
	}
}

func TestAMissingSecretStillGetsANewCA(t *testing.T) {
	s, _, ctx, _, now := newRecordingStore(t)
	got, err := s.Ensure(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := got.Validate(now, s.DNSNames); err != nil {
		t.Error(err)
	}
}

func TestTheProviderKeepsRunningOnARefusedSecretAndPicksUpItsRepair(t *testing.T) {
	s, _, ctx, _, _ := newRecordingStore(t)
	real := issued(t, s)
	seedSecret(t, ctx, s, dataOf(real, issued(t, s).CACertPEM))
	CAKeyUnmatched.Set(0)

	p := NewProvider(s)
	run, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- p.Start(run) }()

	deadline := time.Now().Add(10 * time.Second)
	for testutil.ToFloat64(CAKeyUnmatched) != 1 {
		select {
		case err := <-done:
			t.Fatalf("Start returned %v; the operator must stay up for the alert to be scraped", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("spawnery_ca_key_unmatched never rose")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if p.CABundle() != nil {
		t.Error("a CA bundle is published for a secret that was refused")
	}

	secret := readSecret(t, ctx, s)
	secret.Data["ca.crt"] = real.CACertPEM
	if err := s.Client.Update(ctx, secret); err != nil {
		t.Fatal(err)
	}
	if p.refresh(run, true) {
		t.Error("refresh stays on the short interval after the repair")
	}
	if !bytes.Equal(p.CABundle(), real.CACertPEM) {
		t.Error("the repaired secret was not published")
	}
	if v := testutil.ToFloat64(CAKeyUnmatched); v != 0 {
		t.Errorf("spawnery_ca_key_unmatched = %v after the repair, want 0", v)
	}
	stop()
	if err := <-done; err != nil {
		t.Errorf("Start = %v, want nil on cancel", err)
	}
}

func TestARunningProviderKeepsServingItsLastCertificateThroughARefusal(t *testing.T) {
	s, _, ctx, _, _ := newRecordingStore(t)
	p := NewProvider(s)
	run, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- p.Start(run) }()
	for deadline := time.Now().Add(10 * time.Second); p.CABundle() == nil; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the provider never published a bundle")
		}
	}
	served := p.CABundle()

	secret := readSecret(t, ctx, s)
	secret.Data["ca.crt"] = issued(t, s).CACertPEM
	if err := s.Client.Update(ctx, secret); err != nil {
		t.Fatal(err)
	}
	if !p.refresh(run, false) {
		t.Error("a refused check must go back to the short interval")
	}
	if !bytes.Equal(p.CABundle(), served) {
		t.Error("the provider stopped serving its last good bundle")
	}
	if _, err := p.GetCertificate(nil); err != nil {
		t.Errorf("GetCertificate: %v", err)
	}
	if v := testutil.ToFloat64(CAKeyUnmatched); v != 1 {
		t.Errorf("spawnery_ca_key_unmatched = %v, want 1", v)
	}
	stop()
	<-done
}
