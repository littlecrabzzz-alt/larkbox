package sdk_test

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	larkcontact "github.com/larksuite/oapi-sdk-go/v3/service/contact/v3"

	"github.com/littlecrabzzz-alt/larkbox/internal/config"
	"github.com/littlecrabzzz-alt/larkbox/internal/event"
	"github.com/littlecrabzzz-alt/larkbox/internal/store"
)

func TestOfficialSDKDirectory(t *testing.T) {
	cfg := sdkTestConfig()
	app, alice, bob := &cfg.Apps[0], &cfg.Users[0], &cfg.Users[1]
	app.RedirectURIs = append(app.RedirectURIs, callbackURL)
	engineering := cfg.Departments[0]
	support := config.Department{OpenID: "od-support", ID: "support", Name: "Support", ParentOpenID: engineering.OpenID}
	executive := config.Department{OpenID: "od-executive", ID: "executive", Name: "Executive"}
	foreign := config.Department{OpenID: "od-other", ID: "executive", Name: "Other", TenantKey: "other"}
	private := config.User{OpenID: "ou_private", UserID: "private", Name: "Private User", Email: "private@example.test", DepartmentOpenIDs: []string{executive.OpenID}}
	cfg.Departments = append(cfg.Departments, support, executive, foreign)
	app.ContactScope = &config.ContactScope{DepartmentOpenIDs: []string{engineering.OpenID}}
	alice.ContactScope = &config.ContactScope{UserOpenIDs: []string{private.OpenID}}
	bob.EmployeeNo, bob.Mobile = "EMP-002", "+8613800000000"
	cfg.Users = append(cfg.Users, private, config.User{OpenID: "ou_foreign", UserID: "foreign", Name: bob.Name, Email: bob.Email, TenantKey: "other", DepartmentOpenIDs: []string{foreign.OpenID}})
	srv, web := sdkEventServer(t, cfg, event.Options{})
	client := sdkEventREST(web, *app)
	ctx := t.Context()

	resolved, err := client.Contact.User.BatchGetId(ctx, larkcontact.NewBatchGetIdUserReqBuilder().UserIdType("open_id").
		Body(larkcontact.NewBatchGetIdUserReqBodyBuilder().Emails([]string{"BOB@EXAMPLE.TEST", private.Email}).Mobiles([]string{bob.Mobile}).Build()).Build())
	requireSDKNetwork(t, err)
	if !resolved.Success() || resolved.Data == nil || len(resolved.Data.UserList) != 3 || value(resolved.Data.UserList[0].UserId) != bob.OpenID ||
		resolved.Data.UserList[1].UserId != nil || value(resolved.Data.UserList[2].UserId) != bob.OpenID {
		t.Fatalf("incorrect scoped recipient resolution: %s", resolved.RawBody)
	}

	detail, err := client.Contact.User.Get(ctx, larkcontact.NewGetUserReqBuilder().UserId(bob.UnionID).UserIdType("union_id").DepartmentIdType("department_id").Build())
	requireSDKNetwork(t, err)
	if !detail.Success() || detail.Data == nil || detail.Data.User == nil || value(detail.Data.User.EmployeeNo) != bob.EmployeeNo ||
		len(detail.Data.User.DepartmentIds) != 1 || detail.Data.User.DepartmentIds[0] != engineering.ID {
		t.Fatalf("incorrect employee lookup: %s", detail.RawBody)
	}

	first, err := client.Contact.User.FindByDepartment(ctx, larkcontact.NewFindByDepartmentUserReqBuilder().
		DepartmentId(engineering.OpenID).PageSize(1).Build())
	requireSDKNetwork(t, err)
	if !first.Success() || first.Data == nil || !value(first.Data.HasMore) || len(first.Data.Items) != 1 || value(first.Data.Items[0].OpenId) != alice.OpenID {
		t.Fatalf("incorrect department member page: %s", first.RawBody)
	}
	second, err := client.Contact.User.FindByDepartment(ctx, larkcontact.NewFindByDepartmentUserReqBuilder().
		DepartmentId(engineering.OpenID).PageSize(1).PageToken(value(first.Data.PageToken)).Build())
	requireSDKNetwork(t, err)
	if !second.Success() || second.Data == nil || value(second.Data.HasMore) || len(second.Data.Items) != 1 || value(second.Data.Items[0].OpenId) != bob.OpenID {
		t.Fatalf("incorrect second department page: %s", second.RawBody)
	}
	departments, err := client.Contact.Department.Children(ctx, larkcontact.NewChildrenDepartmentReqBuilder().DepartmentId(engineering.ID).
		DepartmentIdType("department_id").Build())
	requireSDKNetwork(t, err)
	if !departments.Success() || departments.Data == nil || len(departments.Data.Items) != 1 || value(departments.Data.Items[0].OpenDepartmentId) != support.OpenID {
		t.Fatalf("incorrect child departments: %s", departments.RawBody)
	}
	visibleScope, err := client.Contact.Scope.List(ctx, larkcontact.NewListScopeReqBuilder().Build())
	requireSDKNetwork(t, err)
	if !visibleScope.Success() || visibleScope.Data == nil || len(visibleScope.Data.DepartmentIds) != 1 || visibleScope.Data.DepartmentIds[0] != engineering.OpenID {
		t.Fatalf("incorrect app scope: %s", visibleScope.RawBody)
	}

	const userCredential = "u-directory-alice"
	requireSDKNetwork(t, srv.Store().PutToken(store.Token{Value: userCredential, Kind: store.UserToken,
		AppID: app.AppID, UserOpenID: alice.OpenID, ExpiresAt: time.Now().Add(time.Minute)}))
	selected := larkcontact.NewGetUserReqBuilder().UserId(private.OpenID).Build()
	fromApp, err := client.Contact.User.Get(ctx, selected)
	requireSDKNetwork(t, err)
	if fromApp.Success() {
		t.Fatal("tenant credentials crossed into a hidden department")
	}
	fromUser, err := client.Contact.User.Get(ctx, selected, larkcore.WithUserAccessToken(userCredential))
	requireSDKNetwork(t, err)
	if !fromUser.Success() || fromUser.Data == nil || fromUser.Data.User == nil || value(fromUser.Data.User.OpenId) != private.OpenID {
		t.Fatalf("user visibility ignored its own contact scope: %s", fromUser.RawBody)
	}
	basic, err := client.Contact.User.BasicBatch(ctx, larkcontact.NewBasicBatchUserReqBuilder().UserIdType("open_id").
		Body(larkcontact.NewBasicBatchUserReqBodyBuilder().UserIds([]string{bob.OpenID, private.OpenID}).Build()).Build(), larkcore.WithUserAccessToken(userCredential))
	requireSDKNetwork(t, err)
	if !basic.Success() || basic.Data == nil || len(basic.Data.Users) != 1 || value(basic.Data.Users[0].UserId) != private.OpenID {
		t.Fatalf("scoped basic lookup returned %s", basic.RawBody)
	}
	grant := func(scope string) string {
		params := url.Values{"client_id": {app.AppID}, "redirect_uri": {callbackURL}, "open_id": {alice.OpenID}, "scope": {scope}}
		header, _ := accountRequest(t, web, "/open-apis/authen/v1/authorize?"+params.Encode(), nil, nil, http.StatusFound)
		redirect, err := url.Parse(header.Get("Location"))
		requireSDKNetwork(t, err)
		_, raw := accountRequest(t, web, "/oauth/v3/token", url.Values{
			"grant_type": {"authorization_code"}, "client_id": {app.AppID}, "client_secret": {app.AppSecret},
			"code": {redirect.Query().Get("code")}, "redirect_uri": {callbackURL},
		}, nil, http.StatusOK)
		return value(sandboxDecode[oauthWire](t, raw).AccessToken)
	}
	search := func(credential string, status int) string {
		t.Helper()
		query, err := http.NewRequestWithContext(ctx, http.MethodPost, web.URL+"/open-apis/contact/v3/users/search", strings.NewReader(`{"query":"Bob"}`))
		requireSDKNetwork(t, err)
		query.Header.Set("Authorization", "Bearer "+credential)
		query.Header.Set("Content-Type", "application/json")
		response, err := web.Client().Do(query)
		requireSDKNetwork(t, err)
		defer func() { _ = response.Body.Close() }()
		body, err := io.ReadAll(response.Body)
		requireSDKNetwork(t, err)
		if response.StatusCode != status {
			t.Fatalf("search returned HTTP %d, expected %d: %s", response.StatusCode, status, body)
		}
		return string(body)
	}
	if denied := search(grant("im:message"), http.StatusBadRequest); !strings.Contains(denied, `"code":99991672`) || !strings.Contains(denied, `"subject":"contact:user:search"`) {
		t.Fatalf("scope denial omitted the missing permission: %s", denied)
	}
	if allowed := search(grant("contact:user:search"), http.StatusOK); !strings.Contains(allowed, `"items":[]`) {
		t.Fatalf("search ignored the caller's directory visibility: %s", allowed)
	}
}
