package store

import (
	"errors"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/littlecrabzzz-alt/larkbox/internal/config"
	"github.com/littlecrabzzz-alt/larkbox/internal/protocol"
)

// Approval errors identify the failing entity or transition.
var (
	ErrApprovalDefinition = errors.New("approval definition not found")
	ErrApprovalActor      = errors.New("approval actor is not permitted")
	ErrApprovalTask       = errors.New("approval task not found")
	ErrApprovalFinished   = errors.New("approval is already complete")
	ErrApprovalDepartment = errors.New("approval applicant is outside the selected department")
)

// ApprovalStatus is a native approval instance state.
type ApprovalStatus string

// Approval starts pending and ends with one decision or withdrawal.
const (
	ApprovalPending  ApprovalStatus = "PENDING"
	ApprovalApproved ApprovalStatus = "APPROVED"
	ApprovalRejected ApprovalStatus = "REJECTED"
	ApprovalCanceled ApprovalStatus = "CANCELED"
)

type approvalRequest struct {
	appID, tenantKey, uuid string
}

type approvalReplay struct {
	input ApprovalStart
	code  string
}

// ApprovalInstance is the current state for one submitted approval.
type ApprovalInstance struct {
	Code, TaskID, TenantKey, DefinitionCode, DefinitionName string
	Form, UUID, DepartmentID, Comment                       string
	Submitter, Approver                                     User
	Status                                                  ApprovalStatus
	StartedAt, CompletedAt                                  time.Time
}

// ApprovalStart contains the definition, applicant, form, and optional retry key.
type ApprovalStart struct {
	DefinitionCode, UserID, OpenID, DepartmentID, Form, UUID string
}

// ApprovalDecision identifies the actor and target for a terminal transition.
type ApprovalDecision struct {
	DefinitionCode, InstanceCode, ActorID, IDType, TaskID, Comment string
	Status                                                         ApprovalStatus
}

// ApprovalChange captures the committed instance and its subscribed receivers.
type ApprovalChange struct {
	Instance    ApprovalInstance
	Subscribers []string
}

// ApprovalDefinitions returns the current fixture definitions for inspection.
func (s *Store) ApprovalDefinitions() []config.ApprovalDefinition {
	return s.approvalDefinitions.List()
}

// SubscribeApproval enables or removes an app's subscription to a definition.
func (s *Store) SubscribeApproval(appID, code string, enabled bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.approvalDefinition(appID, code); err != nil {
		return err
	}
	if enabled {
		if s.approvalSubscribers[code] == nil {
			s.approvalSubscribers[code] = make(map[string]bool)
		}
		s.approvalSubscribers[code][appID] = true
	} else {
		delete(s.approvalSubscribers[code], appID)
	}
	return nil
}

// StartApproval commits a pending approval and its task together.
func (s *Store) StartApproval(appID string, input ApprovalStart) (ApprovalChange, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	app, found := s.apps.items[appID]
	if !found {
		return ApprovalChange{}, ErrApprovalDefinition
	}
	key := approvalRequest{appID, app.TenantKey, input.UUID}
	if replay, exists := s.approvalRequests[key]; input.UUID != "" && exists {
		if replay.input != input {
			return ApprovalChange{}, ErrAlreadyExists
		}
		return ApprovalChange{Instance: cloneApproval(s.approvalInstances[replay.code])}, nil
	}
	definition, err := s.approvalDefinition(appID, input.DefinitionCode)
	if err != nil {
		return ApprovalChange{}, err
	}
	submitter := s.users.items[input.OpenID]
	if input.UserID != "" {
		for _, user := range s.users.items {
			if user.UserID == input.UserID && user.TenantKey == definition.TenantKey {
				submitter = user
				break
			}
		}
		if submitter.UserID != input.UserID {
			return ApprovalChange{}, ErrApprovalActor
		}
	}
	approver := s.users.items[definition.ApproverOpenID]
	if submitter.OpenID == "" || approver.OpenID == "" || submitter.TenantKey != definition.TenantKey ||
		approver.TenantKey != definition.TenantKey || submitter.Inactive || approver.Inactive {
		return ApprovalChange{}, ErrApprovalActor
	}
	department := ""
	for _, id := range submitter.DepartmentOpenIDs {
		if candidate := s.departments.items[id].ID; candidate != "" && (input.DepartmentID == "" || candidate == input.DepartmentID) {
			department = candidate
			break
		}
	}
	if input.DepartmentID != "" && department == "" {
		return ApprovalChange{}, ErrApprovalDepartment
	}
	instance := ApprovalInstance{
		Code: approvalCode(), TaskID: protocol.NewID(protocol.CardID), Status: ApprovalPending, StartedAt: s.Now(),
		DefinitionCode: definition.Code, DefinitionName: definition.Name, TenantKey: definition.TenantKey,
		Form: input.Form, UUID: input.UUID, DepartmentID: department, Submitter: cloneUser(submitter), Approver: cloneUser(approver),
	}
	s.approvalInstances[instance.Code] = instance
	if input.UUID != "" {
		s.approvalRequests[key] = approvalReplay{input: input, code: instance.Code}
	}
	return s.approvalChange(instance), nil
}

// ReadApproval returns an independent snapshot of an instance in the app's tenant.
func (s *Store) ReadApproval(appID, code string) (ApprovalInstance, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	instance, err := s.approvalInstance(appID, code)
	return cloneApproval(instance), err
}

// DecideApproval completes the single pending task or withdraws its instance.
func (s *Store) DecideApproval(appID string, input ApprovalDecision) (ApprovalChange, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	instance, err := s.approvalInstance(appID, input.InstanceCode)
	if err != nil {
		return ApprovalChange{}, err
	}
	if instance.DefinitionCode != input.DefinitionCode {
		return ApprovalChange{}, ErrApprovalDefinition
	}
	actor := instance.Approver
	switch input.Status {
	case ApprovalCanceled:
		actor = instance.Submitter
	case ApprovalApproved, ApprovalRejected:
		if input.TaskID != instance.TaskID {
			return ApprovalChange{}, ErrApprovalTask
		}
	default:
		return ApprovalChange{}, ErrApprovalFinished
	}
	current, exists := s.users.items[actor.OpenID]
	if !exists || current.Inactive || current.TenantKey != instance.TenantKey || input.ActorID == "" || current.ContactID(input.IDType) != input.ActorID {
		return ApprovalChange{}, ErrApprovalActor
	}
	if instance.Status != ApprovalPending {
		return ApprovalChange{}, ErrApprovalFinished
	}
	instance.Status, instance.Comment, instance.CompletedAt = input.Status, input.Comment, s.Now()
	s.approvalInstances[instance.Code] = instance
	return s.approvalChange(instance), nil
}

// approvalDefinition, approvalInstance, and approvalChange require the store lock.
func (s *Store) approvalDefinition(appID, code string) (config.ApprovalDefinition, error) {
	app, ok := s.apps.items[appID]
	definition, found := s.approvalDefinitions.items[code]
	if !ok || !found || definition.TenantKey != app.TenantKey {
		return config.ApprovalDefinition{}, ErrApprovalDefinition
	}
	return definition, nil
}

func (s *Store) approvalInstance(appID, code string) (ApprovalInstance, error) {
	app, ok := s.apps.items[appID]
	instance, found := s.approvalInstances[code]
	if !ok || !found || instance.TenantKey != app.TenantKey {
		return ApprovalInstance{}, ErrNotFound
	}
	return instance, nil
}

func (s *Store) approvalChange(instance ApprovalInstance) ApprovalChange {
	result := ApprovalChange{Instance: cloneApproval(instance)}
	for _, appID := range slices.Sorted(maps.Keys(s.approvalSubscribers[instance.DefinitionCode])) {
		if app, ok := s.apps.items[appID]; ok && app.TenantKey == instance.TenantKey {
			result.Subscribers = append(result.Subscribers, appID)
		}
	}
	return result
}

func cloneApproval(instance ApprovalInstance) ApprovalInstance {
	instance.Submitter, instance.Approver = cloneUser(instance.Submitter), cloneUser(instance.Approver)
	return instance
}

func approvalCode() string {
	hex := strings.ToUpper(protocol.NewID(""))
	return hex[:8] + "-" + hex[8:12] + "-" + hex[12:16] + "-" + hex[16:20] + "-" + hex[20:]
}
