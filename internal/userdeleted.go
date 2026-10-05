package internal

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Muxcore-Media/contracts-media/events"
	"github.com/Muxcore-Media/userdata-local/internal/store"
)

// ApplyUserDeleted deletes the account userdata blob and profile siblings
// described by an identity.user.deleted payload.
func ApplyUserDeleted(st *store.Store, payload []byte) error {
	if st == nil {
		return fmt.Errorf("store is nil")
	}
	var body events.UserDeletedPayload
	if err := json.Unmarshal(payload, &body); err != nil {
		return fmt.Errorf("decode identity.user.deleted: %w", err)
	}
	if strings.TrimSpace(body.UserID) == "" {
		return fmt.Errorf("identity.user.deleted missing user_id")
	}
	if _, err := st.DeleteAccount(body.UserID); err != nil {
		return err
	}
	return nil
}
