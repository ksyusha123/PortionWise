package bot

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"fooddiary/internal/core"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const Schema = `CREATE TABLE IF NOT EXISTS sessions(telegram_id INTEGER PRIMARY KEY,payload TEXT NOT NULL);CREATE TABLE IF NOT EXISTS cursor(id INTEGER PRIMARY KEY CHECK(id=1),offset INTEGER NOT NULL);INSERT OR IGNORE INTO cursor VALUES(1,0);`

type Session struct {
	User   core.User  `json:"user"`
	Token  string     `json:"token"`
	Issued int64      `json:"issued"`
	Step   string     `json:"step"`
	Draft  *core.Meal `json:"draft,omitempty"`
	EditID string     `json:"edit_id,omitempty"`
}
type Bot struct {
	DB                                          *sql.DB
	TG                                          *Telegram
	Client                                      *core.Client
	UserURL, AnalysisURL, DiaryURL, InternalKey string
}

func (b *Bot) load(ctx context.Context, id int64) (Session, error) {
	var s Session
	var raw string
	e := b.DB.QueryRowContext(ctx, "SELECT payload FROM sessions WHERE telegram_id=?", id).Scan(&raw)
	if errors.Is(e, sql.ErrNoRows) {
		return s, nil
	}
	if e == nil {
		e = json.Unmarshal([]byte(raw), &s)
	}
	return s, e
}
func (b *Bot) save(ctx context.Context, id int64, s Session) error {
	raw, e := json.Marshal(s)
	if e != nil {
		return e
	}
	_, e = b.DB.ExecContext(ctx, "INSERT INTO sessions(telegram_id,payload) VALUES(?,?) ON CONFLICT(telegram_id) DO UPDATE SET payload=excluded.payload", id, string(raw))
	return e
}
func (b *Bot) login(ctx context.Context, from TGUser, s *Session) error {
	if s.Token != "" && time.Now().Unix()-s.Issued < 20*3600 {
		return nil
	}
	raw, _ := json.Marshal(map[string]any{"telegram_id": from.ID, "name": from.FirstName})
	req, e := http.NewRequestWithContext(ctx, "POST", b.UserURL+"/internal/auth/telegram", bytes.NewReader(raw))
	if e != nil {
		return e
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Internal-Key", b.InternalKey)
	res, e := b.Client.HTTP.Do(req)
	if e != nil {
		return errors.New("сервис пользователей недоступен")
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return errors.New("не удалось войти")
	}
	var v struct {
		User  core.User `json:"user"`
		Token string    `json:"token"`
	}
	if json.NewDecoder(res.Body).Decode(&v) != nil || v.Token == "" {
		return errors.New("не удалось прочитать профиль")
	}
	s.User = v.User
	s.Token = v.Token
	s.Issued = time.Now().Unix()
	return nil
}
func (b *Bot) Run(ctx context.Context) error {
	// Fail early on an invalid token or an existing webhook; don't silently delete user configuration.
	if e := b.TG.call(ctx, "getMe", map[string]any{}, nil); e != nil {
		return e
	}
	var webhook struct {
		URL string `json:"url"`
	}
	if e := b.TG.call(ctx, "getWebhookInfo", map[string]any{}, &webhook); e != nil {
		return e
	}
	if webhook.URL != "" {
		return errors.New("у бота настроен webhook: удалите его перед запуском long polling")
	}
	var offset int64
	if e := b.DB.QueryRowContext(ctx, "SELECT offset FROM cursor WHERE id=1").Scan(&offset); e != nil {
		return e
	}
	slog.Info("Telegram long polling started")
	for ctx.Err() == nil {
		var updates []Update
		e := b.TG.call(ctx, "getUpdates", map[string]any{"offset": offset, "timeout": 25, "limit": 10, "allowed_updates": []string{"message", "callback_query"}}, &updates)
		if e != nil {
			if ctx.Err() != nil {
				return nil
			}
			slog.Warn("Telegram polling failed")
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(3 * time.Second):
			}
			continue
		}
		for _, u := range updates {
			if u.ID < offset {
				continue
			}
			if e = b.handle(ctx, u); e != nil {
				return e
			}
			offset = u.ID + 1
			if _, e = b.DB.ExecContext(ctx, "UPDATE cursor SET offset=? WHERE id=1", offset); e != nil {
				return e
			}
		}
	}
	return nil
}
func (b *Bot) handle(ctx context.Context, u Update) error {
	m := u.Message
	from := TGUser{}
	action := ""
	if u.Callback != nil {
		m = u.Callback.Message
		from = u.Callback.From
		action = u.Callback.Data
		_ = b.TG.call(ctx, "answerCallbackQuery", map[string]string{"callback_query_id": u.Callback.ID}, nil)
	} else if m != nil {
		from = m.From
	}
	if m == nil || m.Chat.Type != "private" || from.ID != m.Chat.ID {
		return nil
	}
	s, e := b.load(ctx, from.ID)
	if e != nil {
		return e
	}
	if e = b.login(ctx, from, &s); e != nil {
		slog.Warn("Bot login failed", "update_id", u.ID, "error", e.Error())
		_ = b.TG.send(ctx, m.Chat.ID, e.Error()+". Попробуйте позднее.", nil)
		return nil
	}
	e = b.dispatch(ctx, u.ID, m, action, &s)
	// Persist drafts even when Telegram delivery fails; old buttons are checked against current state.
	if saveErr := b.save(ctx, from.ID, s); saveErr != nil {
		return saveErr
	}
	if e != nil {
		slog.Warn("Bot action failed", "update_id", u.ID, "action", action, "error", e.Error())
		_ = b.TG.send(ctx, m.Chat.ID, "Не удалось выполнить действие: "+e.Error(), keys([]Button{button("Меню", "menu"), button("Отмена", "cancel")}))
	}
	return nil
}
func (b *Bot) api(ctx context.Context, method, path, token string, in, out any) error {
	var body *bytes.Reader
	if in == nil {
		body = bytes.NewReader(nil)
	} else {
		raw, _ := json.Marshal(in)
		body = bytes.NewReader(raw)
	}
	return b.Client.Do(ctx, method, path, token, body, out)
}
func nutrition(n core.Nutrition) string {
	return fmt.Sprintf("%.0f ккал · Б %.1f г · Ж %.1f г · У %.1f г", n.Calories, n.Protein, n.Fat, n.Carbs)
}
func foodText(f core.Food) string {
	var t strings.Builder
	t.WriteString(f.Name + "\n")
	for i, v := range f.Items {
		fmt.Fprintf(&t, "%d. %s — %.0f г\n", i+1, v.Name, v.Grams)
	}
	t.WriteString("\n" + nutrition(f.Nutrition))
	if f.Note != "" {
		t.WriteString("\n\n" + f.Note)
	}
	text := t.String()
	if len([]rune(text)) > 3200 {
		text = string([]rune(text)[:3200])
	}
	return text
}
func parseNutrition(text string) (core.Nutrition, error) {
	parts := strings.Fields(strings.ReplaceAll(text, ",", "."))
	if len(parts) != 4 {
		return core.Nutrition{}, errors.New("введите 4 числа: ккал белки жиры углеводы. Например: 450 30 15 50")
	}
	var a [4]float64
	for i, p := range parts {
		v, e := strconv.ParseFloat(p, 64)
		if e != nil {
			return core.Nutrition{}, errors.New("ожидались числа")
		}
		a[i] = v
	}
	n := core.Nutrition{Calories: a[0], Protein: a[1], Fat: a[2], Carbs: a[3]}
	if !n.Valid() {
		return n, errors.New("введите неотрицательные конечные значения КБЖУ")
	}
	return n, nil
}
func (b *Bot) draft(ctx context.Context, chat int64, s *Session) error {
	if s.Draft == nil {
		return errors.New("черновика нет; начните добавление заново")
	}
	s.Step = "draft"
	if e := s.Draft.Food.Validate(); e != nil {
		return e
	}
	t, _ := time.Parse(time.RFC3339, s.Draft.EatenAt)
	text := foodText(s.Draft.Food) + "\n\nДата: " + t.In(s.User.Location()).Format("2006-01-02 15:04") + "\nОценку ИИ нужно проверить перед сохранением."
	return b.TG.send(ctx, chat, text, keys([]Button{button("Сохранить", "save"), button("Отмена", "cancel")}, []Button{button("Название", "name"), button("Вес", "weights")}, []Button{button("Состав", "items"), button("КБЖУ порции", "nut")}, []Button{button("Дата и время", "when")}))
}
func (b *Bot) dispatch(ctx context.Context, updateID int64, m *Message, action string, s *Session) error {
	chat := m.Chat.ID
	text := strings.TrimSpace(m.Text)
	if action == "" && strings.HasPrefix(text, "/") {
		switch strings.SplitN(text, " ", 2)[0] {
		case "/start", "/menu", "/help":
			action = "menu"
		case "/cancel":
			action = "cancel"
		}
	}
	if action != "" {
		return b.action(ctx, chat, action, s)
	}
	if len(m.Photo) > 0 {
		return b.analyze(ctx, updateID, m, s)
	}
	switch s.Step {
	case "photo":
		return b.TG.send(ctx, chat, "Отправьте фотографию блюда (JPEG/PNG до 5 МБ).", nil)
	case "manual_name":
		if text == "" || len([]rune(text)) > 200 {
			return errors.New("название должно содержать от 1 до 200 символов")
		}
		s.Draft.Food.Name = text
		s.Step = "manual_nut"
		return b.TG.send(ctx, chat, "Введите КБЖУ всей съеденной порции четырьмя числами:\nккал белки жиры углеводы\nНапример: 450 30 15 50", nil)
	case "manual_nut", "nut":
		n, e := parseNutrition(text)
		if e != nil {
			return e
		}
		if s.Draft == nil {
			return errors.New("черновика нет")
		}
		s.Draft.Food.Nutrition = n
		s.Draft.Food.Items = nil
		return b.draft(ctx, chat, s)
	case "name":
		if s.Draft == nil {
			return errors.New("черновика нет")
		}
		f := s.Draft.Food
		f.Name = text
		if e := f.Validate(); e != nil {
			return e
		}
		s.Draft.Food = f
		return b.draft(ctx, chat, s)
	case "weights":
		if s.Draft == nil {
			return errors.New("черновика нет")
		}
		v := strings.Fields(strings.ReplaceAll(text, ",", "."))
		if len(v) != 2 {
			return errors.New("введите номер ингредиента и массу, например: 1 180")
		}
		i, e := strconv.Atoi(v[0])
		if e != nil || i < 1 || i > len(s.Draft.Food.Items) {
			return errors.New("неверный номер ингредиента")
		}
		g, e := strconv.ParseFloat(v[1], 64)
		if e != nil || g <= 0 || g > 10000 {
			return errors.New("масса должна быть от 0 до 10000 г")
		}
		f := s.Draft.Food
		f.Items = append([]core.Item(nil), f.Items...)
		f.Items[i-1].Grams = g
		if e = f.Validate(); e != nil {
			return e
		}
		s.Draft.Food = f
		return b.draft(ctx, chat, s)
	case "items":
		if s.Draft == nil {
			return errors.New("черновика нет")
		}
		var items []core.Item
		for _, line := range strings.Split(text, "\n") {
			p := strings.Split(line, ";")
			if len(p) != 6 {
				return errors.New("каждая строка: название;масса;ккал;белки;жиры;углеводы")
			}
			n, e := parseNutrition(strings.Join(p[2:], " "))
			if e != nil {
				return e
			}
			grams, e := strconv.ParseFloat(strings.ReplaceAll(strings.TrimSpace(p[1]), ",", "."), 64)
			if e != nil {
				return errors.New("неверная масса")
			}
			items = append(items, core.Item{Name: strings.TrimSpace(p[0]), Grams: grams, Per100: n})
		}
		f := s.Draft.Food
		f.Items = items
		if e := f.Validate(); e != nil {
			return e
		}
		s.Draft.Food = f
		return b.draft(ctx, chat, s)
	case "when":
		if s.Draft == nil {
			return errors.New("черновика нет")
		}
		t, e := time.ParseInLocation("2006-01-02 15:04", text, s.User.Location())
		if e != nil || t.Year() < 2000 || t.Year() > 2100 {
			return errors.New("формат: 2026-11-16 13:30, годы 2000–2100")
		}
		s.Draft.EatenAt = t.UTC().Format(time.RFC3339)
		return b.draft(ctx, chat, s)
	case "date":
		if _, e := time.Parse("2006-01-02", text); e != nil {
			return errors.New("формат даты: 2026-11-16")
		}
		s.Step = ""
		return b.showDay(ctx, chat, text, 0, s)
	case "target":
		n, e := parseNutrition(text)
		if e != nil {
			return e
		}
		u := s.User
		u.Target = &n
		if e = b.api(ctx, "PUT", b.UserURL+"/me", s.Token, u, &u); e != nil {
			return e
		}
		s.User = u
		s.Step = ""
		return b.TG.send(ctx, chat, "Норма сохранена: "+nutrition(n), menu())
	case "tz":
		if _, e := time.LoadLocation(text); e != nil {
			return errors.New("используйте часовой пояс, например Asia/Yekaterinburg или Europe/Moscow")
		}
		u := s.User
		u.Timezone = text
		if e := b.api(ctx, "PUT", b.UserURL+"/me", s.Token, u, &u); e != nil {
			return e
		}
		s.User = u
		s.Step = ""
		return b.TG.send(ctx, chat, "Часовой пояс: "+text, menu())
	default:
		return b.TG.send(ctx, chat, "Выберите действие в меню или отправьте фотографию еды.", menu())
	}
}
func (b *Bot) action(ctx context.Context, chat int64, a string, s *Session) error {
	switch a {
	case "menu", "cancel":
		s.Step = ""
		s.Draft = nil
		s.EditID = ""
		text := "Выберите действие. Фото: до 5 анализов в день.\nЧасовой пояс: " + s.User.Timezone
		if a == "menu" {
			text += "\nВ настройках укажите свой часовой пояс и суточную норму."
		}
		return b.TG.send(ctx, chat, text, menu())
	case "photo":
		s.Step = "photo"
		s.Draft = nil
		s.EditID = ""
		return b.TG.send(ctx, chat, "Отправьте фотографию блюда. Результат будет приблизительным.", keys([]Button{button("Отмена", "cancel")}))
	case "manual":
		s.Draft = &core.Meal{RequestKey: core.ID(), EatenAt: time.Now().UTC().Format(time.RFC3339)}
		s.EditID = ""
		s.Step = "manual_name"
		return b.TG.send(ctx, chat, "Как называется блюдо?", keys([]Button{button("Отмена", "cancel")}))
	case "today", "remaining":
		s.Step = ""
		return b.showDay(ctx, chat, time.Now().In(s.User.Location()).Format("2006-01-02"), 0, s)
	case "date":
		s.Step = "date"
		return b.TG.send(ctx, chat, "Введите дату: ГГГГ-ММ-ДД", nil)
	case "profile":
		var u core.User
		if e := b.api(ctx, "GET", b.UserURL+"/me", s.Token, nil, &u); e != nil {
			return e
		}
		s.User = u
		t := "Профиль: " + u.Name + "\nЧасовой пояс: " + u.Timezone
		if u.Target != nil {
			t += "\nНорма: " + nutrition(*u.Target)
		}
		var q struct {
			Remaining int `json:"remaining"`
		}
		if b.api(ctx, "GET", b.AnalysisURL+"/quota", s.Token, nil, &q) == nil {
			t += fmt.Sprintf("\nАнализов осталось: %d/5", q.Remaining)
		}
		return b.TG.send(ctx, chat, t, keys([]Button{button("Часовой пояс", "tz"), button("Норма", "target")}, []Button{button("Убрать норму", "clear_target"), button("Меню", "menu")}))
	case "target":
		s.Step = "target"
		return b.TG.send(ctx, chat, "Введите суточную норму: ккал белки жиры углеводы\nНапример: 2000 120 65 230", nil)
	case "clear_target":
		u := s.User
		u.Target = nil
		if e := b.api(ctx, "PUT", b.UserURL+"/me", s.Token, u, &u); e != nil {
			return e
		}
		s.User = u
		return b.TG.send(ctx, chat, "Норма удалена.", menu())
	case "tz":
		s.Step = "tz"
		return b.TG.send(ctx, chat, "Введите часовой пояс: Asia/Yekaterinburg, Europe/Moscow и т. п.", nil)
	case "save":
		if s.Draft == nil {
			return errors.New("черновик уже сохранён или отменён")
		}
		var saved core.Meal
		method, path := "POST", b.DiaryURL+"/meals"
		if s.EditID != "" {
			method = "PUT"
			path += "/" + s.EditID
		}
		if e := b.api(ctx, method, path, s.Token, s.Draft, &saved); e != nil {
			return e
		}
		s.Draft = nil
		s.EditID = ""
		s.Step = ""
		return b.TG.send(ctx, chat, "Сохранено: "+saved.Food.Name+"\n"+nutrition(saved.Food.Nutrition), menu())
	case "name", "nut", "weights", "items", "when":
		if s.Draft == nil {
			return errors.New("черновика нет")
		}
		s.Step = a
		t := ""
		switch a {
		case "name":
			t = "Введите новое название."
		case "nut":
			t = "Введите КБЖУ всей порции: ккал белки жиры углеводы.\nЭто заменит расчёт по ингредиентам; список ингредиентов в текущем черновике будет очищен. Исходный анализ ИИ останется сохранён."
		case "weights":
			if len(s.Draft.Food.Items) == 0 {
				return errors.New("нет ингредиентов: сначала задайте состав")
			}
			t = "Введите номер ингредиента и новую массу, например: 1 180\n\n" + foodText(s.Draft.Food)
		case "items":
			t = "Введите весь исправленный состав, по одному ингредиенту на строку:\nназвание;масса в г;ккал;белки;жиры;углеводы\nКБЖУ — на 100 г готового продукта.\nПример:\nРис;180;130;2.7;0.3;28\nКурица;150;165;31;3.6;0"
		case "when":
			t = "Введите местные дату и время: 2026-11-16 13:30\nЧасовой пояс: " + s.User.Timezone
		}
		return b.TG.send(ctx, chat, t, keys([]Button{button("К черновику", "draft"), button("Отмена", "cancel")}))
	case "draft":
		return b.draft(ctx, chat, s)
	}
	if strings.HasPrefix(a, "day:") {
		p := strings.Split(a, ":")
		if len(p) != 3 {
			return errors.New("неверная дата")
		}
		offset, _ := strconv.Atoi(p[2])
		s.Step = ""
		return b.showDay(ctx, chat, p[1], offset, s)
	}
	p := strings.SplitN(a, ":", 2)
	if len(p) != 2 {
		return errors.New("кнопка устарела")
	}
	id := p[1]
	switch p[0] {
	case "open", "edit", "original":
		var meal core.Meal
		if e := b.api(ctx, "GET", b.DiaryURL+"/meals/"+id, s.Token, nil, &meal); e != nil {
			return e
		}
		if p[0] == "edit" {
			s.Draft = &meal
			s.EditID = meal.ID
			return b.draft(ctx, chat, s)
		}
		if p[0] == "original" {
			if meal.AnalysisSnapshot == nil || meal.AnalysisSnapshot.Food == nil {
				return errors.New("у записи нет исходного анализа")
			}
			return b.TG.send(ctx, chat, "Исходный анализ ИИ:\n"+foodText(*meal.AnalysisSnapshot.Food), keys([]Button{button("К записи", "open:"+id)}))
		}
		t, _ := time.Parse(time.RFC3339, meal.EatenAt)
		k := keys([]Button{button("Изменить", "edit:"+id), button("Удалить", "delete:"+id)}, []Button{button("Меню", "menu")})
		if meal.AnalysisSnapshot != nil {
			k.Rows = append(k.Rows, []Button{button("Исходный анализ", "original:"+id)})
		}
		return b.TG.send(ctx, chat, foodText(meal.Food)+"\n\n"+t.In(s.User.Location()).Format("2006-01-02 15:04"), k)
	case "delete":
		return b.TG.send(ctx, chat, "Удалить эту запись?", keys([]Button{button("Да, удалить", "confirmdelete:"+id), button("Отмена", "open:"+id)}))
	case "confirmdelete":
		if e := b.api(ctx, "DELETE", b.DiaryURL+"/meals/"+id, s.Token, nil, nil); e != nil {
			return e
		}
		if s.EditID == id {
			s.EditID = ""
			s.Draft = nil
			s.Step = ""
		}
		return b.TG.send(ctx, chat, "Запись удалена.", menu())
	}
	return errors.New("неизвестное действие")
}
func (b *Bot) showDay(ctx context.Context, chat int64, date string, offset int, s *Session) error {
	t, e := time.Parse("2006-01-02", date)
	if e != nil {
		return errors.New("неверная дата")
	}
	var d core.Day
	if e = b.api(ctx, "GET", b.DiaryURL+"/days/"+date+"?timezone="+url.QueryEscape(s.User.Timezone), s.Token, nil, &d); e != nil {
		return e
	}
	offset = max(0, offset)
	if offset >= len(d.Meals) {
		offset = 0
	}
	end := min(offset+10, len(d.Meals))
	text := "Дневник · " + date + "\n\nВсего: " + nutrition(d.Total)
	if d.Target != nil {
		text += "\nНорма: " + nutrition(*d.Target) + "\nОсталось: " + nutrition(*d.Remaining) + "\nОтрицательные значения — превышение нормы."
	} else if d.TargetUnavailable {
		text += "\nНорма временно недоступна."
	} else {
		text += "\nСуточная норма пока не задана."
	}
	k := keys()
	if len(d.Meals) == 0 {
		text += "\n\nПриёмов пищи пока нет."
	} else {
		text += fmt.Sprintf("\n\nЗаписи %d–%d из %d:", offset+1, end, len(d.Meals))
	}
	for i := offset; i < end; i++ {
		meal := d.Meals[i]
		mt, _ := time.Parse(time.RFC3339, meal.EatenAt)
		name := meal.Food.Name
		if len([]rune(name)) > 70 {
			name = string([]rune(name)[:70])
		}
		text += fmt.Sprintf("\n%d. %s %s — %.0f ккал", i+1, mt.In(s.User.Location()).Format("15:04"), name, meal.Food.Nutrition.Calories)
		k.Rows = append(k.Rows, []Button{button(fmt.Sprintf("%d. %s", i+1, name), "open:"+meal.ID)})
	}
	if offset > 0 || end < len(d.Meals) {
		var row []Button
		if offset > 0 {
			row = append(row, button("Предыдущие записи", fmt.Sprintf("day:%s:%d", date, max(0, offset-10))))
		}
		if end < len(d.Meals) {
			row = append(row, button("Следующие записи", fmt.Sprintf("day:%s:%d", date, end)))
		}
		k.Rows = append(k.Rows, row)
	}
	k.Rows = append(k.Rows, []Button{button("← День", "day:"+t.AddDate(0, 0, -1).Format("2006-01-02")+":0"), button("Дата", "date"), button("День →", "day:"+t.AddDate(0, 0, 1).Format("2006-01-02")+":0")}, []Button{button("Меню", "menu")})
	return b.TG.send(ctx, chat, text, k)
}
func (b *Bot) analyze(ctx context.Context, updateID int64, m *Message, s *Session) error {
	if s.Draft != nil {
		return b.TG.send(ctx, m.Chat.ID, "Сначала сохраните или отмените текущий черновик.", keys([]Button{button("Черновик", "draft"), button("Отмена", "cancel")}))
	}
	photo := m.Photo[len(m.Photo)-1]
	if photo.Size > 5<<20 {
		return errors.New("фотография должна быть до 5 МБ")
	}
	data, e := b.TG.photo(ctx, photo.ID)
	if e != nil {
		return e
	}
	_ = b.TG.send(ctx, m.Chat.ID, "Анализирую фотографию…", nil)
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, e := writer.CreateFormFile("photo", "photo.jpg")
	if e != nil {
		return e
	}
	if _, e = part.Write(data); e != nil {
		return e
	}
	if e = writer.Close(); e != nil {
		return e
	}
	req, e := http.NewRequestWithContext(ctx, "POST", b.AnalysisURL+"/analyses", &body)
	if e != nil {
		return e
	}
	req.Header.Set("Authorization", "Bearer "+s.Token)
	req.Header.Set("Idempotency-Key", fmt.Sprintf("telegram:%d", updateID))
	req.Header.Set("Content-Type", writer.FormDataContentType())
	client := &http.Client{Timeout: 100 * time.Second}
	res, e := client.Do(req)
	if e != nil {
		return errors.New("сервис анализа недоступен; ручной ввод продолжает работать")
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		var v struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(res.Body).Decode(&v)
		if v.Error == "" {
			v.Error = "ошибка анализа"
		}
		return errors.New(v.Error)
	}
	var a core.Analysis
	if e = json.NewDecoder(res.Body).Decode(&a); e != nil {
		return errors.New("не удалось прочитать анализ")
	}
	if a.Food == nil {
		s.Step = ""
		return b.TG.send(ctx, m.Chat.ID, fmt.Sprintf("Еда не распознана. Эта обработка учтена в лимите. Осталось: %d/5.", a.Remaining), menu())
	}
	s.Draft = &core.Meal{RequestKey: core.ID(), SourceAnalysisID: a.ID, EatenAt: time.Now().UTC().Format(time.RFC3339), Food: *a.Food}
	s.EditID = ""
	if e = b.save(ctx, m.From.ID, *s); e != nil {
		return e
	}
	if e = b.TG.send(ctx, m.Chat.ID, fmt.Sprintf("Анализ готов. Осталось сегодня: %d/5.", a.Remaining), nil); e != nil {
		return e
	}
	return b.draft(ctx, m.Chat.ID, s)
}
