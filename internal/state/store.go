package state

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var ErrInvalid = errors.New("invalid or unsupported state journal")
var ErrConflict = errors.New("state entry changed; reload before retry")

func Decode(raw []byte, maxBytes int) (Store, error) {
	var s Store
	if len(raw) > maxBytes {
		return s, ErrInvalid
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&s); err != nil {
		return Store{}, ErrInvalid
	}
	if d.Decode(new(any)) != io.EOF || s.Version != CurrentVersion || s.Clusters == nil {
		return Store{}, ErrInvalid
	}
	for key, entry := range s.Clusters {
		parts := strings.Split(key, "/")
		if len(parts) != 2 || len(validation.IsDNS1123Label(parts[0])) > 0 || len(validation.IsDNS1123Subdomain(parts[1])) > 0 || entry.ClusterUID == "" {
			return Store{}, ErrInvalid
		}
		if (entry.CurrentLeaseID == "") != (entry.CurrentExpiresAt == nil) {
			return Store{}, ErrInvalid
		}
		if p := entry.Pending; p != nil {
			if p.LeaseID == "" || (p.Username == "" && p.Stage != StageReplacementBackoff) || p.TriggerID == "" || p.ExpiresAt.IsZero() || p.StageDeadline.IsZero() || !ValidStage(p.Stage) {
				return Store{}, ErrInvalid
			}
			if p.NextActionAt != nil && p.Stage != StagePasswordPatched && p.Stage != StageReconnectPending {
				return Store{}, ErrInvalid
			}
		}
	}
	return s, nil
}

func ValidStage(s Stage) bool {
	switch s {
	case StageIssued, StageWaitingForSecret, StagePasswordPatched, StageReconnectPending, StageVerified, StageReplacementBackoff:
		return true
	}
	return false
}

// Journal only accesses the installation-owned system Secret. Compare-and-swap
// guards the entry while fresh resource versions allow unrelated entry updates.
type Journal struct {
	Client   client.Client
	Reader   client.Reader
	Key      client.ObjectKey
	DataKey  string
	MaxBytes int
}

func (j *Journal) Load(ctx context.Context) (Store, error) {
	s := &corev1.Secret{}
	if err := j.Reader.Get(ctx, j.Key, s); err != nil {
		return Store{}, errors.New("state Secret read failed")
	}
	return Decode(s.Data[j.DataKey], j.MaxBytes)
}

func (j *Journal) Put(ctx context.Context, key string, before, after *ClusterState) error {
	for attempt := 0; attempt < 5; attempt++ {
		secret := &corev1.Secret{}
		if err := j.Reader.Get(ctx, j.Key, secret); err != nil {
			return errors.New("state Secret read failed")
		}
		s, err := Decode(secret.Data[j.DataKey], j.MaxBytes)
		if err != nil {
			return err
		}
		entry, exists := s.Clusters[key]
		if (before == nil && exists) || (before != nil && (!exists || !reflect.DeepEqual(*before, entry))) {
			return ErrConflict
		}
		if reflect.DeepEqual(before, after) {
			return nil
		}
		if after == nil {
			delete(s.Clusters, key)
		} else {
			s.Clusters[key] = *after
		}
		raw, err := json.Marshal(s)
		if err != nil {
			return ErrInvalid
		}
		if _, err = Decode(raw, j.MaxBytes); err != nil {
			return err
		}
		secret.Data[j.DataKey] = raw
		if err = j.Client.Update(ctx, secret); apierrors.IsConflict(err) {
			continue
		} else if err != nil {
			return errors.New("state Secret update failed")
		}
		return nil
	}
	return ErrConflict
}
