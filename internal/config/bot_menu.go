package config

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// BotMenu is the effective menu for a seeded bot.
type BotMenu struct {
	Enabled  bool          `json:"bot_menu_enable" yaml:"bot_menu_enable"`
	Strategy int           `json:"bot_menu_display_strategy,omitempty" yaml:"bot_menu_display_strategy,omitempty"`
	Items    []BotMenuItem `json:"bot_menus" yaml:"bot_menus"`
}

// BotMenuItem is a node in the flat menu list accepted by Application v7.
type BotMenuItem struct {
	ID          string            `json:"menu_id" yaml:"menu_id"`
	ParentID    string            `json:"parent_menu_id,omitempty" yaml:"parent_menu_id,omitempty"`
	Sort        int               `json:"sort,omitempty" yaml:"sort,omitempty"`
	Name        string            `json:"default_name" yaml:"default_name"`
	I18nName    map[string]string `json:"i18n_name,omitempty" yaml:"i18n_name,omitempty"`
	Kind        int               `json:"menu_content_type" yaml:"menu_content_type"`
	EventKey    string            `json:"event_key,omitempty" yaml:"event_key,omitempty"`
	IconFileKey string            `json:"icon_file_key,omitempty" yaml:"icon_file_key,omitempty"`
	Redirect    *BotMenuLink      `json:"redirect_link,omitempty" yaml:"redirect_link,omitempty"`
	Icon        *BotMenuIcon      `json:"ud_icon,omitempty" yaml:"ud_icon,omitempty"`
}

// BotMenuLink gives a link action its desktop and mobile destinations.
type BotMenuLink struct {
	PC     string `json:"pc_url,omitempty" yaml:"pc_url,omitempty"`
	Mobile string `json:"mobile_url,omitempty" yaml:"mobile_url,omitempty"`
}

// BotMenuIcon describes an icon from the Feishu icon library.
type BotMenuIcon struct {
	Token string `json:"token,omitempty" yaml:"token,omitempty"`
	Color string `json:"color,omitempty" yaml:"color,omitempty"`
}

// Menu content type values follow the Application v7 SDK.
const (
	MenuLink    = 1
	MenuEvent   = 2
	MenuSubmenu = 3
	MenuMessage = 4
)

// Validate checks identity, nesting and action fields before applying a menu.
func (m BotMenu) Validate() error {
	if m.Enabled && len(m.Items) == 0 {
		return errors.New("an enabled menu requires at least one item")
	}
	items := make(map[string]BotMenuItem, len(m.Items))
	events := make(map[string]bool)
	counts := make(map[string]int)
	for _, item := range m.Items {
		if strings.TrimSpace(item.ID) == "" || strings.TrimSpace(item.Name) == "" || items[item.ID].ID != "" {
			return errors.New("menu items require a unique menu_id and a default_name")
		}
		items[item.ID] = item
		counts[item.ParentID]++
		switch item.Kind {
		case MenuLink:
			if item.Redirect == nil || (item.Redirect.PC == "" && item.Redirect.Mobile == "") {
				return fmt.Errorf("menu item %q requires redirect_link", item.ID)
			}
		case MenuEvent:
			if strings.TrimSpace(item.EventKey) == "" || events[item.EventKey] {
				return fmt.Errorf("menu item %q requires a unique event_key", item.ID)
			}
			events[item.EventKey] = true
		case MenuSubmenu, MenuMessage:
		default:
			return fmt.Errorf("menu item %q has an invalid menu_content_type", item.ID)
		}
	}
	rootLimit := 3
	if m.Strategy > 1 {
		rootLimit = 5
	}
	if counts[""] > rootLimit {
		return fmt.Errorf("menu supports at most %d top-level items for this display strategy", rootLimit)
	}
	for _, item := range m.Items {
		if item.Kind == MenuSubmenu && (item.ParentID != "" || counts[item.ID] == 0 || counts[item.ID] > 5) {
			return fmt.Errorf("submenu %q must be top-level with one to five children", item.ID)
		}
		if item.ParentID != "" && items[item.ParentID].Kind != MenuSubmenu {
			return fmt.Errorf("menu item %q requires a submenu parent", item.ID)
		}
	}
	return nil
}

// Clone returns a menu with independent item configuration.
func (m BotMenu) Clone() BotMenu {
	m.Items = slices.Clone(m.Items)
	for i := range m.Items {
		item := &m.Items[i]
		item.I18nName = maps.Clone(item.I18nName)
		if item.Redirect != nil {
			link := *item.Redirect
			item.Redirect = &link
		}
		if item.Icon != nil {
			icon := *item.Icon
			item.Icon = &icon
		}
	}
	return m
}
