package application

import (
	"context"
	"fmt"
	"maps"
	"math"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"masamong/internal/action"
	"masamong/internal/config"
	"masamong/internal/errcode"
	"masamong/internal/hostprofile"
	"masamong/internal/memory/store"
	"masamong/internal/providers/openai"
	"masamong/internal/providers/websearch"
	"masamong/internal/secrets"
	"masamong/internal/tools"
)

func (s *Service) providers(ctx context.Context, req action.Request) action.Result {
	in := inputMap(req)
	switch req.Action {
	case "providers.list":
		list := s.Registry().Statuses()
		out := make([]any, 0, len(list))
		for _, st := range list {
			out = append(out, st)
		}
		prices := map[string]any{}
		for role, m := range s.Doc().Models {
			if m.Model == "" {
				continue
			}
			if p, src, ok := s.modelPrice(m.Provider, m.Model); ok {
				row := map[string]any{"in_per_mtok": p.In * 1e6, "out_per_mtok": p.Out * 1e6, "source": src}
				if c, ok := s.cachedPricer(m.Provider)(m.Model); ok {
					row["cached_per_mtok"] = c * 1e6
				}
				if pv, ok := s.Doc().Providers[m.Provider]; ok && src == "published" {
					if pub, ok := openai.Published(openai.ResolveAPI(pv.API, pv.BaseURL), m.Model); ok {
						row["checked"] = pub.Checked
					}
				}
				prices[role] = row
			} else {
				prices[role] = map[string]any{"source": "unknown"}
			}
		}
		// what each model provider can do, so the console need not know hosts by name
		caps := map[string]any{}
		for id, p := range s.Doc().Providers {
			if p.Kind == config.KindOpenAI {
				api := openai.ResolveAPI(p.API, p.BaseURL)
				c := map[string]any{"api": api, "rerank": openai.RerankAt(api, p.BaseURL), "prices_in_list": api.PricesInList()}
				if h, ok := openai.HostByURL(p.BaseURL); ok {
					c["host"] = h.ID // a listed host: the console shows its name, note and key page
				}
				caps[id] = c
			}
		}
		return action.OK(req, map[string]any{"providers": out, "kinds": config.ProviderKinds, "credential_values_included": false, "health": s.providerHealth(),
			"model_prices": prices, "keychain": secrets.KeychainSupported(), "hosts": openai.Hosts(), "web_hosts": websearch.Hosts, "model_providers": caps,
			"search_order": s.searchOrder(), "embed_width": s.embedWidthInfo(), "resting_models": s.restingModels()})
	case "providers.add":
		return s.providerAdd(req, in)
	case "providers.remove":
		id := strVal(in, "id", "")
		return s.mutateConfig(req, func(d *config.Document) error {
			p, ok := d.Providers[id]
			if !ok {
				return fmt.Errorf("unknown provider %s", id)
			}
			if p.Kind != config.KindOpenAI && !config.SearchKind(p.Kind) && p.Kind != config.KindJinaReader {
				return fmt.Errorf("providers.%s: only model, search and reader providers are removed here (switch others off instead)", id)
			}
			if i := slices.Index(d.WebSearch.Order, id); i >= 0 {
				d.WebSearch.Order = slices.Delete(slices.Clone(d.WebSearch.Order), i, i+1)
			}
			for role, m := range d.Models {
				if m.Provider == id {
					return fmt.Errorf("providers.%s is used by the %s model; choose another provider for it first", id, role)
				}
			}
			// a fallback on the removed provider goes with it, as its place in the search order does
			for role, m := range d.Models {
				if kept := slices.DeleteFunc(slices.Clone(m.Fallbacks), func(f config.ModelFallback) bool { return f.Provider == id }); len(kept) != len(m.Fallbacks) {
					m.Fallbacks = kept
					d.Models[role] = m
				}
			}
			delete(d.Providers, id)
			return nil
		})
	case "models.recommend":
		return s.modelsRecommend(ctx, req, in)
	case "models.embed_width":
		return s.modelsEmbedWidth(req, in)
	case "providers.test":
		id := strVal(in, "id", strVal(in, "provider", ""))
		if id == "" {
			return action.Fail(req, errcode.InvalidInput, "generic.error", "provider id required", map[string]any{"known": providerIDs(s.Doc())})
		}
		st, ok := s.Registry().Status(id)
		if !ok {
			return action.Fail(req, errcode.InvalidInput, "generic.error", "unknown provider "+id, map[string]any{"known": providerIDs(s.Doc())})
		}
		if !st.Configured {
			return action.Fail(req, errcode.ProviderUnconfigured, "provider.unconfigured", s.msg(req, "provider.unconfigured", "provider", id), map[string]any{"provider": id, "credential_ref": st.CredentialRef})
		}
		out, err := s.Registry().Probe(ctx, id)
		if err == nil {
			err = s.proveModelKey(ctx, id, out)
		}
		if err != nil {
			return s.providerFail(req, id, err)
		}
		s.noteProvider(id, nil)
		out["ok"] = true
		out["live"] = true
		out["credential_ref"] = st.CredentialRef
		return action.OK(req, out)
	case "providers.credentials":
		// Secrets are never accepted through an Action payload; only the reference is stored (11장 §3).
		id := strVal(in, "id", "")
		ref := strVal(in, "credential_ref", "")
		if id == "" || ref == "" {
			return action.Fail(req, errcode.InvalidInput, "generic.error", "id and credential_ref ("+secrets.Hint+") required", nil)
		}
		if _, ok := in["value"]; ok || in["secret"] != nil {
			return action.Fail(req, errcode.InvalidInput, "generic.error", "secret values are not accepted; set the environment variable or file and pass its reference", nil)
		}
		res := s.mutateConfig(req, func(d *config.Document) error {
			p, ok := d.Providers[id]
			if !ok {
				return fmt.Errorf("unknown provider %s", id)
			}
			p.CredentialRef = ref
			d.Providers[id] = p
			return nil
		})
		if res.OK {
			res.Output["value_echoed"] = false
			res.Output["credential_ref"] = ref
			s.credentialsChanged() // the same reference may now hold another key
		}
		return res
	case "models.list":
		id := strVal(in, "provider", "")
		if id == "" {
			for name, p := range s.Doc().Providers {
				if p.Kind == config.KindOpenAI {
					id = name
					break
				}
			}
		}
		a, ok := s.Registry().LLM[id]
		if !ok {
			return action.Fail(req, errcode.InvalidInput, "generic.error", "no openai_compat provider named "+id, nil)
		}
		if !a.Configured() {
			return action.OK(req, map[string]any{"provider": id, "status": "unconfigured", "models": []any{}, "list_is_not_inference": true, "message": s.msg(req, "provider.unconfigured", "provider", id)})
		}
		if err := s.Registry().reserve(id); err != nil {
			return s.providerFail(req, id, err)
		}
		models, err := a.ListModels(ctx)
		if err != nil {
			return s.providerFail(req, id, err)
		}
		s.noteProvider(id, nil) // a listed model list is a working key
		s.storePrices(id, models)
		filter := strings.ToLower(strVal(in, "filter", ""))
		role := strVal(in, "role", "")
		items := []any{}
		price := s.modelListPrice(id)
		for _, m := range models {
			if filter != "" && !strings.Contains(strings.ToLower(m.ID+" "+m.Name), filter) {
				continue
			}
			if (role == "embedding" && m.Kind != "embedding") || (role == "image" && !m.ImageOutput) {
				continue
			}
			pr, priced := price(m)
			items = append(items, modelRow(m, pr, priced))
		}
		return action.OK(req, map[string]any{"provider": id, "status": "ok", "live": true, "models": items, "total": len(models), "list_is_not_inference": true})
	case "models.select":
		role := strVal(in, "role", "response")
		provider := strVal(in, "provider", "")
		model := strVal(in, "model", "")
		if boolVal(in, "clear") {
			// an optional role goes back to unset (vision: the planner and response models read pictures;
			// image: image generation answers "not configured"); the others cannot be empty
			if !slices.Contains(config.OptionalModelRoles, role) {
				return action.Fail(req, errcode.InvalidInput, "generic.error", fmt.Sprintf("only optional roles can be cleared (%v)", config.OptionalModelRoles), map[string]any{"role": role})
			}
			return s.mutateConfig(req, func(d *config.Document) error {
				delete(d.Models, role)
				return nil
			})
		}
		if provider == "" || model == "" {
			return action.Fail(req, errcode.InvalidInput, "generic.error", "provider and model required", nil)
		}
		if !config.ValidModelRole(role) {
			return action.Fail(req, errcode.InvalidInput, "generic.error", fmt.Sprintf("role must be one of %v", config.AllModelRoles()), map[string]any{"role": role})
		}
		// the same rule as models.recommend: while an index exists, a different embedding model is only
		// selected with include_embed=true (the console asks the operator to confirm the re-index the refusal's
		// estimate shows, then sends the request again with include_embed; this is the server side)
		if cur := s.Doc().Models["embed"]; role == "embed" && (cur.Provider != provider || cur.Model != model) && !boolVal(in, "include_embed") {
			if indexed := s.embeddedRooms(); len(indexed) > 0 {
				return embedReindexRequired(req, indexed)
			}
		}
		return s.mutateConfig(req, func(d *config.Document) error {
			if _, ok := d.Providers[provider]; !ok {
				return fmt.Errorf("unknown provider %s", provider)
			}
			selectModel(d, role, provider, model)
			return nil
		})
	}
	return action.Fail(req, errcode.ActionUnknown, "action.unknown", s.msg(req, "action.unknown"), nil)
}

// EmbedReindexRequiredKey is the message_key of the refusal to switch an indexed embedding model without
// include_embed=true. The console recognises it (never the English text) and asks the operator, then
// sends the same request again with include_embed=true; args.reindex is the estimate it shows.
const EmbedReindexRequiredKey = "models.embed_reindex_required"

func embedReindexRequired(req action.Request, indexed map[string]store.RoomText) action.Result {
	return action.Fail(req, errcode.InvalidInput, EmbedReindexRequiredKey,
		"changing the embedding model empties memory search until every room is indexed again: pass include_embed=true to confirm", map[string]any{"reindex": reindexEstimate(indexed, nil)})
}

// modelListPrice resolves a listed model's price the way the cost accounting does (listedPrice: the
// provider's list, then the family's published table, then the public reference list matched by name),
// so the console's model browser and the budget ranking show the same numbers the bot is billed by.
func (s *Service) modelListPrice(provider string) openai.PriceFunc {
	return func(m openai.Model) (openai.ModelPrice, bool) {
		p, src, ok := s.listedPrice(provider, m.ID)
		if !ok {
			return openai.ModelPrice{}, false
		}
		out := openai.ModelPrice{In: p.In * 1e6, Out: p.Out * 1e6, Cached: p.Cached * 1e6, Source: src}
		if src == "list" {
			_, err := strconv.ParseFloat(m.CompletionPrice, 64)
			out.OutKnown = err == nil // a list that gives no output price leaves it unknown, not free
		} else {
			out.OutKnown = p.Out > 0 || m.Kind != "chat"
		}
		return out, true
	}
}

// modelRow is one model of models.list. Prices stay the host's per-token strings (prompt_price,
// completion_price; "unknown" when the list has none) and also come as numbers per million tokens
// (in_per_mtok, out_per_mtok, cached_per_mtok, price_source), resolved by modelListPrice. Each is absent
// when unknown: never 0 for "unknown", and a model whose output price is unknown has no out_per_mtok.
// image_input and supports_tools are true/false when the list says and absent when it does not.
func modelRow(m openai.Model, pr openai.ModelPrice, priced bool) map[string]any {
	price := "unknown"
	if m.PromptPrice != "" {
		price = m.PromptPrice
	}
	row := map[string]any{"id": m.ID, "name": m.Name, "context_length": m.ContextLength, "prompt_price": price, "completion_price": orUnknown(m.CompletionPrice), "kind": m.Kind, "image_output": m.ImageOutput}
	if m.ImageInput != nil {
		row["image_input"] = *m.ImageInput
	}
	if m.SupportsTools != nil {
		row["supports_tools"] = *m.SupportsTools
	}
	if priced {
		row["in_per_mtok"], row["price_source"] = pr.In, pr.Source
		if pr.OutKnown {
			row["out_per_mtok"] = pr.Out
		}
		if pr.Cached > 0 && pr.Cached <= pr.In {
			row["cached_per_mtok"] = pr.Cached
		}
	}
	return row
}

// budgetRows is openai.Budget as the action's output: {role: [alternatives]}, each marked when it is the
// role's current model on this provider.
func (s *Service) budgetRows(id string, budget map[string][]openai.BudgetPick) map[string]any {
	out := map[string]any{}
	for _, role := range openai.BudgetRoles {
		rows := []any{}
		for _, b := range budget[role] {
			cur := s.Doc().Models[role]
			row := map[string]any{"model": b.ID, "name": b.Name, "in_per_mtok": b.InPerMTok, "out_per_mtok": b.OutPerMTok, "blended_per_mtok": b.BlendedPerMTok,
				"context_length": b.ContextLength, "known_family": b.Known, "price_source": b.PriceSource, "current": cur.Provider == id && cur.Model == b.ID}
			if b.CachedPerMTok > 0 {
				row["cached_per_mtok"] = b.CachedPerMTok
			}
			if b.SupportsTools != nil {
				row["supports_tools"] = *b.SupportsTools
			}
			if b.ImageInput != nil {
				row["image_input"] = *b.ImageInput
			}
			rows = append(rows, row)
		}
		if len(rows) > 0 {
			out[role] = rows
		}
	}
	return out
}

// selectModel drops provider/model-specific pricing on either change. Embedding
// dimensions reset only for a different model; switching its provider keeps the
// operator's width because the engine space does not include the provider.
func selectModel(d *config.Document, role, provider, model string) {
	if d.Models == nil {
		d.Models = map[string]config.Model{}
	}
	m := d.Models[role]
	if m.Provider != provider || m.Model != model {
		m.PriceInPerMTok, m.PriceOutPerMTok = 0, 0
		if role == "embed" && m.Model != model {
			m.Dimensions = 0
			if width, ok := openai.EmbedFullWidth(model); ok && width > config.DefaultEmbedDimensions {
				m.Dimensions = config.DefaultEmbedDimensions
			}
		}
	}
	m.Provider, m.Model = provider, model
	// the new model is no longer its own fallback
	m.Fallbacks = slices.DeleteFunc(slices.Clone(m.Fallbacks), func(f config.ModelFallback) bool { return f.Provider == provider && f.Model == model })
	if len(m.Fallbacks) == 0 {
		m.Fallbacks = nil
	}
	d.Models[role] = m
}

var providerIDRE = regexp.MustCompile(`^[a-z][a-z0-9_-]{1,31}$`)

// providerAdd adds a model provider: a listed host by its api (openrouter | openai | gemini | anthropic) or its id
// (host: cometapi | nanogpt), or any OpenAI-compatible address (api compatible, base_url required). The key is stored separately
// (/api/secret or providers.credentials); an environment variable named for the host is used when set.
func (s *Service) providerAdd(req action.Request, in map[string]any) action.Result {
	if kind := strVal(in, "kind", ""); kind != "" && kind != config.KindOpenAI {
		return s.webProviderAdd(req, kind, strVal(in, "id", ""))
	}
	api := openai.API(strVal(in, "api", ""))
	host, listed := openai.HostByID(strVal(in, "host", ""))
	if listed {
		api = host.API // a listed host (CometAPI, NanoGPT) brings its family, address and key variable
	} else if h, ok := openai.HostFor(api); ok {
		host, listed = h, true
	}
	if !openai.ValidAPI(string(api)) || api == "" {
		return action.Fail(req, errcode.InvalidInput, "generic.error", fmt.Sprintf("api must be one of %v, or host one of the listed hosts", openai.APIs), nil)
	}
	p := config.Provider{Kind: config.KindOpenAI, BaseURL: strings.TrimRight(strings.TrimSpace(strVal(in, "base_url", "")), "/")}
	id := strVal(in, "id", "")
	if h, ok := host, listed; ok {
		if p.BaseURL == "" {
			p.BaseURL = h.BaseURL
		}
		if id == "" {
			id = h.ID
		}
		p.CredentialRef = "env:" + h.KeyEnv
	}
	if p.BaseURL == "" || id == "" {
		return action.Fail(req, errcode.InvalidInput, "generic.error", "an OpenAI-compatible provider needs id and base_url", nil)
	}
	if !providerIDRE.MatchString(id) {
		return action.Fail(req, errcode.InvalidInput, "generic.error", "id: 2–32 lowercase letters, digits, - or _", nil)
	}
	if openai.DetectAPI(p.BaseURL) != api {
		p.API = string(api) // said explicitly only when the address alone would not tell
	}
	res := s.mutateConfig(req, func(d *config.Document) error {
		if _, ok := d.Providers[id]; ok {
			return fmt.Errorf("provider %s already exists", id)
		}
		d.Providers[id] = p
		return nil
	})
	if res.OK {
		res.Output["id"] = id
		res.Output["api"] = api
	}
	return res
}

// webProviderAdd adds a web search service or the page reader (websearch.Hosts) with its default
// address, a daily call limit, and the key taken from the host's environment variable until one is
// stored.
func (s *Service) webProviderAdd(req action.Request, kind, id string) action.Result {
	h, ok := websearch.HostFor(kind)
	if !ok {
		return action.Fail(req, errcode.InvalidInput, "generic.error", "kind must be a model provider (api) or one of "+strings.Join(websearch.Kinds, ", ")+", jina_reader", nil)
	}
	if id == "" {
		id = h.ID
	}
	if !providerIDRE.MatchString(id) {
		return action.Fail(req, errcode.InvalidInput, "generic.error", "id: 2–32 lowercase letters, digits, - or _", nil)
	}
	p := config.Provider{Kind: kind, BaseURL: h.BaseURL, CredentialRef: "env:" + h.KeyEnv, DailyLimit: 100}
	res := s.mutateConfig(req, func(d *config.Document) error {
		if _, ok := d.Providers[id]; ok {
			return fmt.Errorf("provider %s already exists", id)
		}
		d.Providers[id] = p
		return nil
	})
	if res.OK {
		res.Output["id"] = id
		res.Output["kind"] = kind
	}
	return res
}

// modelsRecommend lists a provider's models and picks one per role it can serve (openai.Recommend).
// apply=true points those roles at the picks in one change; roles it cannot serve (rerank on OpenAI or
// Gemini) keep their current model. apply with picks ({role: model}, from the preview the operator
// saw, only the roles they kept) applies exactly those without listing the models again.
//
// The embedding model is special: search reads only leaves embedded by the current model, so a new one
// leaves memory empty until everything is embedded again. While an index exists, apply changes it only
// with include_embed=true (the console asks with the preview's reindex estimate); reindex=true then
// starts re-embedding every indexed room at once.
func (s *Service) modelsRecommend(ctx context.Context, req action.Request, in map[string]any) action.Result {
	id := strVal(in, "provider", "")
	p, ok := s.Doc().Providers[id]
	a, aok := s.Registry().LLM[id]
	if !ok || !aok || p.Kind != config.KindOpenAI {
		return action.Fail(req, errcode.InvalidInput, "generic.error", "no model provider named "+id, nil)
	}
	api := openai.ResolveAPI(p.API, p.BaseURL)
	embedChanges := func(picks map[string]string) bool {
		e, ok := picks["embed"]
		cur := s.Doc().Models["embed"]
		return ok && (cur.Provider != id || cur.Model != e)
	}
	// The stored width follows the model string, as in selectModel: the engine space has no provider in
	// it, so the same model on another host keeps the operator's width and gets no width proposal.
	widthChanges := func(picks map[string]string) bool {
		e, ok := picks["embed"]
		return ok && s.Doc().Models["embed"].Model != e
	}
	indexed := s.embeddedRooms()
	apply := func(picks map[string]string, out map[string]any) action.Result {
		changes := embedChanges(picks)
		if changes && len(indexed) > 0 && !boolVal(in, "include_embed") {
			if _, given := in["picks"]; given {
				return embedReindexRequired(req, indexed)
			}
			delete(picks, "embed") // recommend --apply keeps the indexed embedding model unless asked
			changes = false
			out["embed_kept"] = true
		}
		delete(out, "embed_dimensions")
		delete(out, "embed_dimensions_reason")
		delete(out, "embed_full_width")
		delete(out, "embed_width_without_rerank_pick")
		embedDimensions, embedDimensionsReason, hasEmbedDimensions := s.recommendationEmbedDimensions(
			picks, id, hostprofile.Current().Tier == hostprofile.TierSmall,
		)
		widthChanged := widthChanges(picks)
		if widthChanged && hasEmbedDimensions {
			setRecommendedEmbedWidth(out, picks["embed"], embedDimensions, embedDimensionsReason)
		}
		res := s.mutateConfig(req, func(d *config.Document) error {
			for role, model := range picks {
				if config.ValidModelRole(role) {
					differs := role == "embed" && d.Models["embed"].Model != model
					selectModel(d, role, id, model)
					if differs && hasEmbedDimensions {
						m := d.Models[role]
						m.Dimensions = embedDimensions
						d.Models[role] = m
					}
				}
			}
			return nil
		})
		if !res.OK {
			return res
		}
		for k, v := range out {
			res.Output[k] = v
		}
		res.Output["provider"], res.Output["applied"], res.Output["embed_changes"] = id, true, changes
		if changes && len(indexed) > 0 && boolVal(in, "reindex") {
			res.Output["reindex_jobs"] = s.reindexAll(indexed, picks["embed"], id)
		}
		return res
	}
	if given, _ := in["picks"].(map[string]any); boolVal(in, "apply") && len(given) > 0 {
		picks := map[string]string{}
		for role, v := range given {
			model, _ := v.(string)
			if !config.ValidModelRole(role) || strings.TrimSpace(model) == "" {
				return action.Fail(req, errcode.InvalidInput, "generic.error", "picks: {role: model} with known roles", map[string]any{"role": role})
			}
			picks[role] = strings.TrimSpace(model)
		}
		return apply(picks, map[string]any{})
	}
	if !a.Configured() {
		return action.Fail(req, errcode.ProviderUnconfigured, "provider.unconfigured", s.msg(req, "provider.unconfigured", "provider", id), map[string]any{"provider": id})
	}
	if err := s.Registry().reserve(id); err != nil {
		return s.providerFail(req, id, err)
	}
	models, err := a.ListModels(ctx)
	if err != nil {
		return s.providerFail(req, id, err)
	}
	s.noteProvider(id, nil)
	picks := openai.Recommend(api, p.BaseURL, models)
	changes := []any{}
	for _, role := range config.AllModelRoles() {
		model, ok := picks[role]
		if !ok {
			continue
		}
		cur := s.Doc().Models[role]
		row := map[string]any{"role": role, "model": model, "current_provider": cur.Provider, "current_model": cur.Model,
			"changed": cur.Provider != id || cur.Model != model}
		if pr, src, ok := s.modelPrice(id, model); ok {
			row["in_per_mtok"], row["out_per_mtok"], row["price_source"] = pr.In*1e6, pr.Out*1e6, src
		}
		changes = append(changes, row)
	}
	s.storePrices(id, models)
	budget := s.budgetRows(id, openai.Budget(api, p.BaseURL, models, s.modelListPrice(id)))
	if len(changes) == 0 && len(budget) == 0 {
		return action.Fail(req, errcode.NoRecommendation, "models.recommend.none", s.msg(req, "models.recommend.none"),
			map[string]any{"provider": id, "reason": s.msg(req, "models.recommend.none.reason")})
	}
	out := map[string]any{"provider": id, "api": api, "picks": changes, "rerank": openai.RerankAt(api, p.BaseURL), "embed_changes": embedChanges(picks),
		"budget": budget, "embed_indexed": len(indexed) > 0}
	small := hostprofile.Current().Tier == hostprofile.TierSmall
	if dimensions, reason, ok := s.recommendationEmbedDimensions(picks, id, small); ok && widthChanges(picks) {
		setRecommendedEmbedWidth(out, picks["embed"], dimensions, reason)
		// The operator may untick the rerank pick, and the width the server would then apply differs when
		// that pick is what makes reranking usable: offer the matching proposal for that case too.
		if _, picked := picks["rerank"]; picked {
			without := maps.Clone(picks)
			delete(without, "rerank")
			if d, r, ok := s.recommendationEmbedDimensions(without, id, small); ok {
				out["embed_width_without_rerank_pick"] = embedWidthProposal(picks["embed"], d, r)
			}
		}
	}
	if embedChanges(picks) && len(indexed) > 0 {
		var price *float64
		if pr, _, ok := s.modelPrice(id, picks["embed"]); ok {
			v := pr.In
			price = &v
		}
		out["reindex"] = reindexEstimate(indexed, price)
	}
	if !boolVal(in, "apply") {
		return action.OK(req, out)
	}
	return apply(picks, out)
}

// recommendationEmbedDimensions returns the width proposed alongside a known MRL embedding pick.
// Unknown models intentionally have no proposed width, so applying them keeps their full width.
func (s *Service) recommendationEmbedDimensions(picks map[string]string, provider string, smallHost bool) (int, string, bool) {
	model := picks["embed"]
	if model == "" {
		return 0, "", false
	}
	width, known := openai.EmbedFullWidth(model)
	if !known {
		return 0, "", false
	}

	if width <= config.DefaultEmbedDimensions {
		return 0, "default", true
	}
	// Assess the document the operator will apply, including retained roles on
	// other providers. A model name alone does not make reranking available.
	doc := s.Doc()
	doc.Models = maps.Clone(doc.Models)
	for role, picked := range picks {
		if config.ValidModelRole(role) {
			selectModel(&doc, role, provider, picked)
		}
	}
	if !s.rerankUsable(doc) {
		if smallHost {
			return config.DefaultEmbedDimensions, "small_host", true
		}
		return 0, "no_rerank", true
	}
	return config.DefaultEmbedDimensions, "default", true
}

func setRecommendedEmbedWidth(out map[string]any, model string, dimensions int, reason string) {
	out["embed_dimensions"], out["embed_dimensions_reason"] = dimensions, reason
	if dimensions == 0 {
		out["embed_full_width"], _ = openai.EmbedFullWidth(model)
	}
}

// embedWidthProposal is one width proposal as a unit: dimensions (0 = the model's full width, then
// full_width says how many), and why.
func embedWidthProposal(model string, dimensions int, reason string) map[string]any {
	out := map[string]any{"dimensions": dimensions, "reason": reason}
	if dimensions == 0 {
		out["full_width"], _ = openai.EmbedFullWidth(model)
	}
	return out
}

// embeddedRooms is what the memory index holds per room (empty without an index).
func (s *Service) embeddedRooms() map[string]store.RoomText {
	eng, err := s.MemoryEngine()
	if err != nil {
		return nil
	}
	rooms, err := eng.Store.EmbeddedRooms()
	if err != nil {
		return nil
	}
	return rooms
}

// reindexEstimate is what embedding every indexed leaf again takes: leaves, tokens (one per non-ASCII
// character, a quarter per ASCII one, as openai.EstimateTokens counts; the character total is used as
// an upper bound) and, with the new model's price per token, its cost.
func reindexEstimate(rooms map[string]store.RoomText, pricePerToken *float64) map[string]any {
	var leaves int
	var chars int64
	for _, t := range rooms {
		leaves += t.Leaves
		chars += t.Chars
	}
	out := map[string]any{"rooms": len(rooms), "leaves": leaves, "tokens": chars}
	if pricePerToken != nil {
		out["usd"] = float64(chars) * *pricePerToken
	}
	return out
}

// reindexAll starts a background prepare job per indexed room so memory search comes back as soon as
// possible under the new embedding model. Each job's budget covers its room at the new price with room
// to spare, and never less than the usual indexing budget.
func (s *Service) reindexAll(rooms map[string]store.RoomText, model, provider string) []string {
	base := parseUSD(s.Doc().Memory.IndexBudgetUSD, 1.0)
	perToken := 0.0
	if pr, _, ok := s.modelPrice(provider, model); ok {
		perToken = pr.In
	}
	names := make([]string, 0, len(rooms))
	for r := range rooms {
		names = append(names, r)
	}
	sort.Strings(names)
	var ids []string
	for _, room := range names {
		if _, ok := s.blockBySource(room); !ok {
			continue // a live room without a block is re-embedded by its own resync
		}
		budget := max(base, math.Ceil(float64(rooms[room].Chars)*perToken*1.5*100)/100)
		if j, _, err := s.startPrepareJob(preparePayload{SourceRoom: room, BudgetUSD: min(budget, 1000)}); err == nil {
			ids = append(ids, j.ID)
		}
	}
	return ids
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

func providerIDs(d config.Document) []string {
	out := make([]string, 0, len(d.Providers))
	for k := range d.Providers {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// mutateConfig applies a typed change through the same CAS path as config.apply, so GUI/CLI edits and
// file edits share one revision history (06장 §3).
func (s *Service) mutateConfig(req action.Request, fn func(*config.Document) error) action.Result {
	// Held from reading the live document to committing the change: a concurrent edit (the console and
	// a Discord command at once) waits instead of being overwritten by a change built on stale data.
	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()
	live := s.snapshot()
	doc := live.doc
	// deep copy maps so a failed mutation never leaks into the live document
	doc.Providers = map[string]config.Provider{}
	for k, v := range live.doc.Providers {
		doc.Providers[k] = v
	}
	doc.Models = map[string]config.Model{}
	for k, v := range live.doc.Models {
		doc.Models[k] = v
	}
	doc.Features = map[string]bool{}
	for k, v := range live.doc.Features {
		doc.Features[k] = v
	}
	if live.doc.Tools != nil {
		doc.Tools = make(map[string]tools.Settings, len(live.doc.Tools))
		for k, v := range live.doc.Tools {
			doc.Tools[k] = v
		}
	}
	doc.Guilds = cloneGuilds(live.doc.Guilds)
	doc.Personas = map[string]config.Persona{}
	for k, v := range live.doc.Personas {
		doc.Personas[k] = v
	}
	if err := fn(&doc); err != nil {
		return action.Fail(req, errcode.InvalidInput, "generic.error", err.Error(), nil)
	}
	raw, err := config.Marshal(doc)
	if err != nil {
		return action.Fail(req, errcode.IO, "generic.error", err.Error(), nil)
	}
	base := live.rev
	if req.BaseRevision > 0 {
		base = req.BaseRevision
	}
	return s.applyCandidateLocked(req, raw, base)
}

func cloneGuilds(in map[string]config.GuildSettings) map[string]config.GuildSettings {
	if in == nil {
		return map[string]config.GuildSettings{}
	}
	out := make(map[string]config.GuildSettings, len(in))
	for gid, g := range in {
		ng := config.GuildSettings{Locale: g.Locale}
		if g.Channels != nil {
			ng.Channels = make(map[string]config.ChannelPolicy, len(g.Channels))
			for cid, ch := range g.Channels {
				cp := ch
				if ch.RAGAttachments != nil {
					cp.RAGAttachments = append([]config.RAGAttachmentRef{}, ch.RAGAttachments...)
				}
				if ch.Tools != nil {
					cp.Tools = make(map[string]bool, len(ch.Tools))
					for k, v := range ch.Tools {
						cp.Tools[k] = v
					}
				}
				ng.Channels[cid] = cp
			}
		}
		out[gid] = ng
	}
	return out
}

func (s *Service) stats(req action.Request) action.Result {
	if req.Action == "reports.create" {
		return s.reportCreate(req)
	}
	// The read pool, not the single writer connection: the console now shows these totals, and a count over
	// hundreds of thousands of messages must not make captures and replies wait on a 1-core box.
	db, err := s.adminDB()
	if err != nil {
		return action.Fail(req, errcode.IO, "generic.error", err.Error(), nil)
	}
	r := db.Reader()
	var polls, jobs, messages int
	_ = r.QueryRow(`SELECT COUNT(*) FROM polls`).Scan(&polls)
	_ = r.QueryRow(`SELECT COUNT(*) FROM jobs`).Scan(&jobs)
	_ = r.QueryRow(`SELECT COUNT(*) FROM memory_observations WHERE deleted_at=''`).Scan(&messages)
	return action.OK(req, map[string]any{
		"window": "all_time", "polls": polls, "jobs": jobs, "messages": messages, "denominator": "stored_rows",
		"note": "counts are exact stored table sizes",
	})
}

// proveModelKey: a model host whose list opens without a key (NanoGPT) accepted a wrong key in the
// connection test. There the chat model a role uses on it is asked for one token; out["key_check"]
// says how the key was proven (list | chat), or that it could not be yet (unverified: no role uses the
// host, the first real call will show).
func (s *Service) proveModelKey(ctx context.Context, id string, out map[string]any) error {
	a := s.Registry().LLM[id]
	p := s.Doc().Providers[id]
	if a == nil || p.Kind != config.KindOpenAI || a.API == openai.APIOpenRouter {
		return nil
	}
	if !a.ListIsPublic(ctx) {
		out["key_check"] = "list"
		return nil
	}
	for _, role := range []string{"response", "planner", "summary", "vision"} {
		if m := s.Doc().Models[role]; m.Provider == id && m.Model != "" {
			out["key_check"] = "chat"
			return a.ProbeChat(ctx, m.Model)
		}
	}
	out["key_check"] = "unverified"
	return nil
}
