package config

import (
	"cmp"
	"slices"
	"strings"
	"unicode/utf8"
)

// NormalizeSettings validates chat settings and fills the Open API defaults.
func (chat *Chat) NormalizeSettings() (field, description string) {
	chat.ChatMode = cmp.Or(chat.ChatMode, "group")
	chat.ChatType = cmp.Or(chat.ChatType, "private")
	switch {
	case chat.ChatMode != "group" && chat.ChatMode != "p2p":
		return "chat_mode", "Expected group or p2p."
	case chat.ChatType != "private" && chat.ChatType != "public":
		return "chat_type", "Expected private or public."
	case utf8.RuneCountInString(chat.Name) > 60 || (chat.ChatType == "public" && utf8.RuneCountInString(strings.TrimSpace(chat.Name)) < 2):
		return "name", "Expected at most 60 characters and at least two for a public group."
	case utf8.RuneCountInString(chat.Description) > 100:
		return "description", "Expected at most 100 characters."
	}
	for _, option := range []struct {
		field   string
		value   *string
		allowed []string
	}{
		{"group_message_type", &chat.GroupMessageType, []string{"chat", "thread"}},
		{"join_message_visibility", &chat.JoinMessageVisibility, []string{"all_members", "only_owner", "not_anyone"}},
		{"leave_message_visibility", &chat.LeaveMessageVisibility, []string{"all_members", "only_owner", "not_anyone"}},
		{"membership_approval", &chat.MembershipApproval, []string{"no_approval_required", "approval_required"}},
		{"urgent_setting", &chat.UrgentSetting, []string{"all_members", "only_owner"}},
		{"video_conference_setting", &chat.VideoConferenceSetting, []string{"all_members", "only_owner"}},
		{"edit_permission", &chat.EditPermission, []string{"all_members", "only_owner"}},
		{"pin_manage_setting", &chat.PinManageSetting, []string{"all_members", "only_owner"}},
		{"hide_member_count_setting", &chat.HideMemberCountSetting, []string{"all_members", "only_owner"}},
	} {
		*option.value = cmp.Or(*option.value, option.allowed[0])
		if !slices.Contains(option.allowed, *option.value) {
			return option.field, "Expected one of: " + strings.Join(option.allowed, ", ") + "."
		}
	}
	if chat.ChatMode == "group" {
		chat.Name = cmp.Or(strings.TrimSpace(chat.Name), "Group Chat")
	}
	for lang := range chat.I18nNames {
		if !slices.Contains([]string{"zh_cn", "en_us", "ja_jp"}, lang) {
			return "i18n_names", "Expected zh_cn, en_us, or ja_jp."
		}
	}
	return "", ""
}
