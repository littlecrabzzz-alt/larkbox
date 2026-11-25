package im

import (
	"errors"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/littlecrabzzz-alt/larkbox/internal/protocol"
	"github.com/littlecrabzzz-alt/larkbox/internal/store"
)

var reactionMessageMissing = protocol.APIError{Code: 231003, Message: "The message is not found, maybe not exist or deleted.", Status: http.StatusBadRequest}
var reactionMissing = protocol.APIError{Code: 231001, Message: "The reaction does not exist.", Status: http.StatusBadRequest}

var emojiType = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{0,63}$`)

// Feishu documents these case-sensitive values for message reactions.
// https://open.feishu.cn/document/server-docs/im-v1/message-reaction/emojis-introduce
var reactionEmojis = strings.Fields(`
	18X 2022 ANGRY APPLAUSE AWESOMEN Alarm BEAR BEER BETRAYED BLUBBER BLUSH BOMB BULL Basketball
	BeamingFace BubbleTea CAKE CALF CANDIEDHAWS CHUCKLE CLAP CLEAVER COMFORT CRAZY CRY CUCUMBER
	CheckMark ClownFace Coffee ColdSweat CrossMark DIZZY DONE DROOL DROWSY DULL Delighted Drumstick
	EMBARRASSED ENOUGH ERROR EYESCLOSED EatingFood FACEPALM FINGERHEART FIRECRACKER FIREWORKS FISTBUMP
	FORTUNE FROWN Fire FullMoonFace GIFT GLANCE GeneralBusinessTrip GeneralDoNotDisturb
	GeneralInMeetingBusy GeneralMoonRest GeneralSun GeneralTravellingCar GeneralWorkFromHome Get GoGoGo
	HAMMER HAUGHTY HEADSET HEART HEARTBROKEN HIGHFIVE HUG HUSKY HappyDragon Hundred INNOCENTSMILE JIAYI
	JOYFUL JubilantRabbit KISS LAUGH LGTM LIPS LOL LOOKDOWN LOVE LUCK Lemon Loudspeaker MONEY MUSCLE
	MeMeMe MinusOne MoonRabbit Mooncake Movie Music NOSEPICK No OBSESSED OK OKR OnIt OneSecond PARTY
	PETRIFIED POOP PRAISE PROUD PUKE Partying Pepper Pin Pumpkin RAINBOWPUKE REDPACKET ROSE RoarForYou
	SALUTE SCOWL SHAKE SHHH SHOCKED SHOWOFF SHY SICK SILENT SKULL SLAP SLEEP SLIGHT SMART SMILE SMIRK
	SMOOCH SMUG SOB SPEECHLESS SPITBLOOD STRIVE SWEAT SaluteFace Shrug Sigh Snowman Soccer StatusBus
	StatusEnjoyLife StatusFlashOfInspiration StatusInFlight StatusReading Status_PrivateMessage
	StickyRiceBalls TEARS TEASE TERROR THANKS THINKING THUMBSUP TOASTED TONGUE TRICK TV ThanksFace
	ThumbsDown Trophy Typing UPPERLEFT VRHeadset WAIL WAVE WHAT WHIMPER WINK WITTY WOW WRONGED XBLUSH
	XmasHat XmasTree YAWN YEAH Yes YouAreTheBest
`)

func checkReactionEmoji(w http.ResponseWriter, field, emoji string) bool {
	if slices.Contains(reactionEmojis, emoji) {
		return true
	}
	if emojiType.MatchString(emoji) {
		fail(w, 231001, "Reaction type is invalid.")
	} else {
		badField(w, field, "Expected an emoji name such as THUMBSUP.")
	}
	return false
}

func (h handler) createReaction(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ReactionType struct {
			EmojiType string `json:"emoji_type"`
		} `json:"reaction_type"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	emoji := req.ReactionType.EmojiType
	if !checkReactionEmoji(w, "reaction_type.emoji_type", emoji) {
		return
	}
	actor := caller(r)
	reaction := store.Reaction{ID: protocol.NewID(protocol.ReactionID), OperatorOpenID: actor.OpenID, EmojiType: emoji}
	if !actor.IsUser {
		reaction.OperatorAppID = actor.AppID
	}
	var receivedBy []string
	var created bool
	err := h.state.Mutate(r.Context(), func() error {
		_, recipients, err := h.state.ChangeIMMessageAndRecipients(actor, r.PathValue("message_id"), func(m *store.Message, _ store.Chat) error {
			if m.MsgType == "system" {
				return protocol.APIError{Code: 231017, Message: "This operation is not supported for this message type.", Status: http.StatusBadRequest}
			}
			for _, existing := range m.Reactions {
				if existing.OperatorAppID == reaction.OperatorAppID && existing.OperatorOpenID == actor.OpenID && existing.EmojiType == emoji {
					reaction = existing
					return nil
				}
			}
			m.ReactionOrder++
			reaction.Order, reaction.CreatedAt = m.ReactionOrder, h.state.Now()
			m.Reactions = append(m.Reactions, reaction)
			created = true
			return nil
		})
		receivedBy = recipients
		return err
	})
	if err != nil {
		reactionError(w, err)
		return
	}
	if created {
		h.emitReaction(r, "im.message.reaction.created_v1", actor, reaction, receivedBy)
	}
	success(w, h.reactionData(reaction, "open_id"))
}

func (h handler) deleteReaction(w http.ResponseWriter, r *http.Request) {
	actor := caller(r)
	id := r.PathValue("reaction_id")
	var reaction store.Reaction
	var receivedBy []string
	err := h.state.Mutate(r.Context(), func() error {
		_, recipients, err := h.state.ChangeIMMessageAndRecipients(actor, r.PathValue("message_id"), func(m *store.Message, _ store.Chat) error {
			index := slices.IndexFunc(m.Reactions, func(candidate store.Reaction) bool { return candidate.ID == id })
			if index < 0 {
				return reactionMissing
			}
			reaction = m.Reactions[index]
			if reaction.OperatorOpenID != actor.OpenID || (reaction.OperatorAppID == "") != actor.IsUser ||
				(!actor.IsUser && reaction.OperatorAppID != actor.AppID) {
				return noPermission
			}
			m.Reactions = slices.Delete(m.Reactions, index, index+1)
			return nil
		})
		receivedBy = recipients
		return err
	})
	if err != nil {
		reactionError(w, err)
		return
	}
	h.emitReaction(r, "im.message.reaction.deleted_v1", actor, reaction, receivedBy)
	success(w, h.reactionData(reaction, "open_id"))
}

func (h handler) emitReaction(r *http.Request, kind string, actor store.IMActor, reaction store.Reaction, recipients []string) {
	now := h.state.Now()
	payload := fields{
		"message_id": r.PathValue("message_id"), "reaction_type": fields{"emoji_type": reaction.EmojiType},
		"action_time": strconv.FormatInt(now.UnixMilli(), 10),
	}
	if actor.IsUser {
		payload["operator_type"] = "user"
		payload["user_id"] = h.eventIdentity(actor.OpenID)
	} else {
		payload["operator_type"] = "app"
		payload["app_id"] = actor.AppID
	}
	for _, target := range recipients {
		h.emitter.Emit(r.Context(), Event{
			Type: kind, AppID: target, MessageID: r.PathValue("message_id"), ActorID: actor.OpenID, ActorAppID: actor.AppID,
			At: now, Payload: payload,
		})
	}
}

func (h handler) listReactions(w http.ResponseWriter, r *http.Request) {
	kind, ok := userIDType(w, r)
	if !ok {
		return
	}
	size, ok := pageSize(w, r, 50)
	if !ok {
		return
	}
	m, err := h.state.IMMessage(caller(r), r.PathValue("message_id"))
	if err != nil {
		reactionError(w, err)
		return
	}
	if m.Deleted {
		reactionError(w, store.ErrMessageDeleted)
		return
	}
	reactions := emptySlice(m.Reactions)
	if values, present := r.URL.Query()["reaction_type"]; present {
		if len(values) != 1 {
			badField(w, "reaction_type", "Expected one emoji name such as THUMBSUP.")
			return
		}
		if !checkReactionEmoji(w, "reaction_type", values[0]) {
			return
		}
		reactions = slices.DeleteFunc(reactions, func(reaction store.Reaction) bool { return reaction.EmojiType != values[0] })
	}
	cursor, ok := readCursor(w, r)
	if !ok {
		return
	}
	result := paginate(reactions, size, cursor, false, func(reaction store.Reaction) sortKey {
		return sortKey{Order: reaction.Order, ID: reaction.ID}
	})
	items := make([]fields, 0, len(result.Items))
	for _, reaction := range result.Items {
		items = append(items, h.reactionData(reaction, kind))
	}
	success(w, page[fields]{Items: items, HasMore: result.HasMore, PageToken: result.PageToken})
}

func (h handler) reactionData(reaction store.Reaction, kind string) fields {
	operator := fields{"operator_id": reaction.OperatorOpenID, "operator_type": "user"}
	if reaction.OperatorAppID != "" {
		operator["operator_id"], operator["operator_type"] = reaction.OperatorAppID, "app"
	} else if user, found := h.state.Users().Get(reaction.OperatorOpenID); found {
		operator["operator_id"] = userID(user, kind)
	}
	return fields{
		"reaction_id": reaction.ID, "operator": operator, "action_time": milliseconds(reaction.CreatedAt),
		"reaction_type": fields{"emoji_type": reaction.EmojiType},
	}
}

func reactionError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrOutsideChat):
		fail(w, 231002, "The operator has no permission to react on the specific message.")
	case errors.Is(err, store.ErrMessageDeleted):
		_ = protocol.WriteError(w, reactionMessageMissing)
	default:
		writeFailure(w, err, reactionMessageMissing)
	}
}
