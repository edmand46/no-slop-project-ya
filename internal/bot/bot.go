// Package bot wires the Telegram update loop to the todo store.
package bot

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	telegram "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/edmand46/no-slop-project-ya/internal/parser"
	"github.com/edmand46/no-slop-project-ya/internal/store"
)

const (
	pageSize        = 10
	maxTaskRunes    = 200
	reminderEvery   = 30 * time.Second
	reminderMessage = "🔔 Напоминание"
)

var noButtons = &models.InlineKeyboardMarkup{InlineKeyboard: [][]models.InlineKeyboardButton{}}

type draft struct {
	id      int64
	text    string
	photo   string
	waiting bool
}

type Bot struct {
	api   *telegram.Bot
	store *store.Store
	loc   *time.Location

	mu     sync.Mutex
	drafts map[int64]*draft
	seq    int64
}

func New(token string, st *store.Store, loc *time.Location) (*Bot, error) {
	b := &Bot{
		store:  st,
		loc:    loc,
		drafts: make(map[int64]*draft),
	}

	api, err := telegram.New(token, telegram.WithDefaultHandler(b.handleText))
	if err != nil {
		return nil, err
	}
	b.api = api

	api.RegisterHandler(telegram.HandlerTypeMessageText, "start", telegram.MatchTypeCommandStartOnly, b.handleStart)
	api.RegisterHandler(telegram.HandlerTypeMessageText, "help", telegram.MatchTypeCommandStartOnly, b.handleHelp)
	api.RegisterHandler(telegram.HandlerTypeMessageText, "add", telegram.MatchTypeCommandStartOnly, b.handleAdd)
	api.RegisterHandler(telegram.HandlerTypeMessageText, "list", telegram.MatchTypeCommandStartOnly, b.handleList)
	api.RegisterHandler(telegram.HandlerTypeMessageText, "cancel", telegram.MatchTypeCommandStartOnly, b.handleCancel)

	api.RegisterHandler(telegram.HandlerTypeCallbackQueryData, "w:", telegram.MatchTypePrefix, b.handleWhen)
	api.RegisterHandler(telegram.HandlerTypeCallbackQueryData, "l:", telegram.MatchTypePrefix, b.handlePage)
	api.RegisterHandler(telegram.HandlerTypeCallbackQueryData, "t:", telegram.MatchTypePrefix, b.handleListDone)
	api.RegisterHandler(telegram.HandlerTypeCallbackQueryData, "d:", telegram.MatchTypePrefix, b.handleListDelete)
	api.RegisterHandler(telegram.HandlerTypeCallbackQueryData, "p:", telegram.MatchTypePrefix, b.handlePhotoView)
	api.RegisterHandler(telegram.HandlerTypeCallbackQueryData, "rt:", telegram.MatchTypePrefix, b.handleReminderDone)
	api.RegisterHandler(telegram.HandlerTypeCallbackQueryData, "rd:", telegram.MatchTypePrefix, b.handleReminderDelete)

	return b, nil
}

func (b *Bot) Run(ctx context.Context) {
	_, err := b.api.SetMyCommands(ctx, &telegram.SetMyCommandsParams{
		Commands: []models.BotCommand{
			{Command: "start", Description: "Начать"},
			{Command: "add", Description: "Добавить задачу"},
			{Command: "list", Description: "Список задач"},
			{Command: "cancel", Description: "Отменить добавление"},
			{Command: "help", Description: "Как пользоваться"},
		},
	})
	if err != nil {
		log.Printf("set commands: %v", err)
	}

	go b.remindersLoop(ctx)
	b.api.Start(ctx)
}

func (b *Bot) handleStart(ctx context.Context, _ *telegram.Bot, update *models.Update) {
	msg, ok := privateMessage(update)
	if !ok {
		return
	}
	b.send(ctx, msg.Chat.ID, `Привет! Я храню ваши задачи и напоминаю о них.

/add купить молоко — добавить задачу
/list — показать список
/help — какие форматы времени я понимаю`, nil)
}

func (b *Bot) handleHelp(ctx context.Context, _ *telegram.Bot, update *models.Update) {
	msg, ok := privateMessage(update)
	if !ok {
		return
	}
	b.send(ctx, msg.Chat.ID, `Как пользоваться:

/add <текст> — добавить задачу, затем выберите время
/list — активные задачи и кнопки «готово» / «удалить»
/cancel — отменить добавление

Фото с подписью — задача с картинкой: подпись станет текстом задачи. В /list у таких задач есть кнопка 📷.

Время можно ввести так:
06.11 18:00
6 ноября в 18:00
завтра в 9:30
послезавтра
в пятницу 18:00
через 2 часа
18:00

Если время не указано, напомню в 09:00. Дата без года в прошлом переносится на следующий год.`, nil)
}

func (b *Bot) handleAdd(ctx context.Context, _ *telegram.Bot, update *models.Update) {
	msg, ok := privateMessage(update)
	if !ok {
		return
	}

	text := strings.TrimSpace(strings.TrimPrefix(msg.Text, "/add"))
	if text == "" {
		b.send(ctx, msg.Chat.ID, "Пришлите текст задачи: /add купить молоко", nil)
		return
	}
	if utf8.RuneCountInString(text) > maxTaskRunes {
		b.send(ctx, msg.Chat.ID, fmt.Sprintf("Слишком длинно: максимум %d символов.", maxTaskRunes), nil)
		return
	}

	d := b.newDraft(msg.Chat.ID, text, "")
	b.send(ctx, msg.Chat.ID, "Когда напомнить?\n"+text, b.whenKeyboard(d))
}

func (b *Bot) handleCancel(ctx context.Context, _ *telegram.Bot, update *models.Update) {
	msg, ok := privateMessage(update)
	if !ok {
		return
	}

	b.mu.Lock()
	_, exists := b.drafts[msg.Chat.ID]
	delete(b.drafts, msg.Chat.ID)
	b.mu.Unlock()

	if exists {
		b.send(ctx, msg.Chat.ID, "Добавление отменено.", nil)
		return
	}
	b.send(ctx, msg.Chat.ID, "Нечего отменять.", nil)
}

func (b *Bot) handleList(ctx context.Context, _ *telegram.Bot, update *models.Update) {
	msg, ok := privateMessage(update)
	if !ok {
		return
	}

	text, markup, err := b.renderList(ctx, msg.Chat.ID, 0)
	if err != nil {
		log.Printf("render list: %v", err)
		b.send(ctx, msg.Chat.ID, "Не получилось загрузить список, попробуйте ещё раз.", nil)
		return
	}
	b.send(ctx, msg.Chat.ID, text, markup)
}

// handleText is the default handler: it handles photos, parses the time for a
// waiting draft, or explains how to add a task.
func (b *Bot) handleText(ctx context.Context, _ *telegram.Bot, update *models.Update) {
	msg, ok := privateMessage(update)
	if !ok {
		return
	}

	if len(msg.Photo) > 0 {
		b.handlePhoto(ctx, msg)
		return
	}

	text := strings.TrimSpace(msg.Text)
	if strings.HasPrefix(text, "/") {
		b.send(ctx, msg.Chat.ID, "Не знаю такую команду. /help", nil)
		return
	}

	d := b.waitingDraft(msg.Chat.ID)
	if d == nil {
		b.send(ctx, msg.Chat.ID, "Чтобы добавить задачу, напишите /add <текст>.", nil)
		return
	}

	due, err := parser.Parse(text, time.Now().In(b.loc))
	if err != nil {
		if errors.Is(err, parser.ErrPast) {
			b.send(ctx, msg.Chat.ID, "Это время уже прошло. Введите будущее время, например: завтра 18:00", nil)
			return
		}
		b.send(ctx, msg.Chat.ID, "Не понял время. Примеры: 06.11 18:00, 6 ноября в 18:00, завтра 9:30, в пятницу 18:00, через 2 часа.", nil)
		return
	}

	task, err := b.store.Add(ctx, msg.Chat.ID, d.text, d.photo, &due)
	if err != nil {
		log.Printf("add task: %v", err)
		b.send(ctx, msg.Chat.ID, "Не получилось сохранить задачу, попробуйте ещё раз.", nil)
		return
	}
	b.removeDraft(msg.Chat.ID, d.id)
	b.sendTask(ctx, msg.Chat.ID, b.addedText(task), task.PhotoFileID, nil)
}

func (b *Bot) handlePhoto(ctx context.Context, msg *models.Message) {
	if len(msg.Photo) == 0 {
		return
	}
	photo := msg.Photo[len(msg.Photo)-1].FileID
	text := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(msg.Caption), "/add"))

	if text == "" {
		if b.attachPhoto(msg.Chat.ID, photo) {
			b.send(ctx, msg.Chat.ID, "📷 Фото прикреплено к задаче.", nil)
			return
		}
		b.send(ctx, msg.Chat.ID, "Подпишите фото текстом задачи: отправьте его с подписью, например «купить молоко».", nil)
		return
	}
	if utf8.RuneCountInString(text) > maxTaskRunes {
		b.send(ctx, msg.Chat.ID, fmt.Sprintf("Слишком длинно: максимум %d символов.", maxTaskRunes), nil)
		return
	}

	d := b.newDraft(msg.Chat.ID, text, photo)
	b.send(ctx, msg.Chat.ID, "Когда напомнить?\n"+text, b.whenKeyboard(d))
}

func (b *Bot) handleWhen(ctx context.Context, _ *telegram.Bot, update *models.Update) {
	cq, msg, ok := callbackContext(update)
	if !ok {
		return
	}

	parts := strings.SplitN(cq.Data, ":", 3)
	if len(parts) != 3 {
		b.answer(ctx, cq, "Некорректная кнопка", true)
		return
	}
	draftID, ok := parseInt(parts[1])
	if !ok {
		b.answer(ctx, cq, "Некорректная кнопка", true)
		return
	}
	userID := cq.From.ID

	b.mu.Lock()
	d := b.drafts[userID]
	if d == nil || d.id != draftID {
		b.mu.Unlock()
		b.answer(ctx, cq, "Это добавление устарело. Начните заново: /add", true)
		return
	}
	draftText := d.text
	draftPhoto := d.photo
	if parts[2] == "other" {
		d.waiting = true
	}
	b.mu.Unlock()

	if parts[2] == "other" {
		b.answer(ctx, cq, "", false)
		b.edit(ctx, msg.Chat.ID, msg.ID, "Введите время.\nНапример: завтра 18:00, 06.11 18:00, в пятницу 09:30, через 2 часа.", noButtons)
		return
	}

	var due *time.Time
	if parts[2] != "none" {
		sec, err := strconv.ParseInt(parts[2], 10, 64)
		if err != nil {
			b.answer(ctx, cq, "Некорректная кнопка", true)
			return
		}
		t := time.Unix(sec, 0).In(b.loc)
		if !t.After(time.Now()) {
			b.answer(ctx, cq, "Это время уже прошло. Нажмите «другое время».", true)
			return
		}
		due = &t
	}

	task, err := b.store.Add(ctx, userID, draftText, draftPhoto, due)
	if err != nil {
		log.Printf("add task: %v", err)
		b.answer(ctx, cq, "Не получилось сохранить задачу", true)
		return
	}
	b.removeDraft(userID, draftID)
	b.answer(ctx, cq, "", false)
	b.edit(ctx, msg.Chat.ID, msg.ID, b.addedText(task), noButtons)
	if task.PhotoFileID != "" {
		b.sendPhoto(ctx, msg.Chat.ID, task.PhotoFileID, fmt.Sprintf("📷 Фото к задаче #%d", task.ID), nil)
	}
}

func (b *Bot) handlePage(ctx context.Context, _ *telegram.Bot, update *models.Update) {
	cq, msg, ok := callbackContext(update)
	if !ok {
		return
	}

	parts := strings.SplitN(cq.Data, ":", 2)
	page, _ := strconv.Atoi(parts[1])

	text, markup, err := b.renderList(ctx, cq.From.ID, page)
	if err != nil {
		log.Printf("render list: %v", err)
		b.answer(ctx, cq, "Не получилось загрузить список", true)
		return
	}
	b.answer(ctx, cq, "", false)
	b.edit(ctx, msg.Chat.ID, msg.ID, text, markupOrEmpty(markup))
}

func (b *Bot) handleListDone(ctx context.Context, _ *telegram.Bot, update *models.Update) {
	cq, msg, ok := callbackContext(update)
	if !ok {
		return
	}

	page, taskID, ok := listCallback(cq.Data)
	if !ok {
		b.answer(ctx, cq, "Некорректная кнопка", true)
		return
	}
	userID := cq.From.ID

	task, err := b.store.Get(ctx, userID, taskID)
	switch {
	case errors.Is(err, store.ErrNoTask):
		b.answer(ctx, cq, "Задача уже удалена", true)
		b.refreshList(ctx, msg, userID, page)
		return
	case err != nil:
		log.Printf("get task: %v", err)
		b.answer(ctx, cq, "Ошибка, попробуйте ещё раз", true)
		return
	case task.Done:
		b.answer(ctx, cq, "Уже готово", false)
		b.refreshList(ctx, msg, userID, page)
		return
	}

	if _, err := b.store.Done(ctx, userID, taskID); err != nil {
		log.Printf("done task: %v", err)
		b.answer(ctx, cq, "Ошибка, попробуйте ещё раз", true)
		return
	}
	b.answer(ctx, cq, "Готово ✅", false)
	b.refreshList(ctx, msg, userID, page)
}

func (b *Bot) handleListDelete(ctx context.Context, _ *telegram.Bot, update *models.Update) {
	cq, msg, ok := callbackContext(update)
	if !ok {
		return
	}

	page, taskID, ok := listCallback(cq.Data)
	if !ok {
		b.answer(ctx, cq, "Некорректная кнопка", true)
		return
	}
	userID := cq.From.ID

	_, err := b.store.Delete(ctx, userID, taskID)
	switch {
	case errors.Is(err, store.ErrNoTask):
		b.answer(ctx, cq, "Задача уже удалена", true)
		b.refreshList(ctx, msg, userID, page)
		return
	case err != nil:
		log.Printf("delete task: %v", err)
		b.answer(ctx, cq, "Ошибка, попробуйте ещё раз", true)
		return
	}
	b.answer(ctx, cq, "Удалено", false)
	b.refreshList(ctx, msg, userID, page)
}

func (b *Bot) handleReminderDone(ctx context.Context, _ *telegram.Bot, update *models.Update) {
	cq, msg, ok := callbackContext(update)
	if !ok {
		return
	}
	taskID, ok := parseInt(strings.TrimPrefix(cq.Data, "rt:"))
	if !ok {
		b.answer(ctx, cq, "Некорректная кнопка", true)
		return
	}

	task, err := b.store.Get(ctx, cq.From.ID, taskID)
	switch {
	case errors.Is(err, store.ErrNoTask):
		b.answer(ctx, cq, "Задача уже удалена", true)
		b.editReminder(ctx, msg, "Задача уже удалена")
		return
	case err != nil:
		log.Printf("get task: %v", err)
		b.answer(ctx, cq, "Ошибка, попробуйте ещё раз", true)
		return
	}

	if !task.Done {
		if _, err := b.store.Done(ctx, cq.From.ID, taskID); err != nil {
			log.Printf("done task: %v", err)
			b.answer(ctx, cq, "Ошибка, попробуйте ещё раз", true)
			return
		}
	}
	b.answer(ctx, cq, "Готово ✅", false)
	b.editReminder(ctx, msg, "✅ Готово: "+task.Text)
}

func (b *Bot) handleReminderDelete(ctx context.Context, _ *telegram.Bot, update *models.Update) {
	cq, msg, ok := callbackContext(update)
	if !ok {
		return
	}
	taskID, ok := parseInt(strings.TrimPrefix(cq.Data, "rd:"))
	if !ok {
		b.answer(ctx, cq, "Некорректная кнопка", true)
		return
	}

	task, err := b.store.Delete(ctx, cq.From.ID, taskID)
	switch {
	case errors.Is(err, store.ErrNoTask):
		b.answer(ctx, cq, "Задача уже удалена", true)
		b.editReminder(ctx, msg, "Задача уже удалена")
		return
	case err != nil:
		log.Printf("delete task: %v", err)
		b.answer(ctx, cq, "Ошибка, попробуйте ещё раз", true)
		return
	}
	b.answer(ctx, cq, "Удалено", false)
	b.editReminder(ctx, msg, "🗑 Удалено: "+task.Text)
}

func (b *Bot) handlePhotoView(ctx context.Context, _ *telegram.Bot, update *models.Update) {
	cq, msg, ok := callbackContext(update)
	if !ok {
		return
	}

	page, taskID, ok := listCallback(cq.Data)
	if !ok {
		b.answer(ctx, cq, "Некорректная кнопка", true)
		return
	}

	task, err := b.store.Get(ctx, cq.From.ID, taskID)
	switch {
	case errors.Is(err, store.ErrNoTask):
		b.answer(ctx, cq, "Задача уже удалена", true)
		b.refreshList(ctx, msg, cq.From.ID, page)
		return
	case err != nil:
		log.Printf("get task: %v", err)
		b.answer(ctx, cq, "Ошибка, попробуйте ещё раз", true)
		return
	}
	if task.PhotoFileID == "" {
		b.answer(ctx, cq, "У задачи нет фото", true)
		return
	}

	b.answer(ctx, cq, "", false)
	b.sendPhoto(ctx, cq.From.ID, task.PhotoFileID, fmt.Sprintf("#%d %s", task.ID, task.Text), nil)
}

func (b *Bot) refreshList(ctx context.Context, msg *models.Message, userID int64, page int) {
	text, markup, err := b.renderList(ctx, userID, page)
	if err != nil {
		log.Printf("render list: %v", err)
		return
	}
	b.edit(ctx, msg.Chat.ID, msg.ID, text, markupOrEmpty(markup))
}

func (b *Bot) renderList(ctx context.Context, userID int64, page int) (string, models.ReplyMarkup, error) {
	count, err := b.store.CountActive(ctx, userID)
	if err != nil {
		return "", nil, err
	}
	if count == 0 {
		return "Список пуст. Добавьте задачу:\n/add купить молоко", nil, nil
	}

	pages := (count + pageSize - 1) / pageSize
	if page < 0 {
		page = 0
	}
	if page >= pages {
		page = pages - 1
	}

	tasks, err := b.store.ListActive(ctx, userID, page*pageSize, pageSize)
	if err != nil {
		return "", nil, err
	}

	var sb strings.Builder
	if pages == 1 {
		sb.WriteString("Ваши задачи:")
	} else {
		first := page*pageSize + 1
		last := first + len(tasks) - 1
		fmt.Fprintf(&sb, "Ваши задачи (%d–%d из %d):", first, last, count)
	}
	for _, task := range tasks {
		fmt.Fprintf(&sb, "\n\n#%d ", task.ID)
		if task.PhotoFileID != "" {
			sb.WriteString("📷 ")
		}
		sb.WriteString(task.Text)
		if task.DueAt != nil {
			fmt.Fprintf(&sb, " — до %s", b.fmtDue(*task.DueAt))
		}
	}

	rows := make([][]models.InlineKeyboardButton, 0, len(tasks)+1)
	for _, task := range tasks {
		row := []models.InlineKeyboardButton{
			{Text: fmt.Sprintf("✅ #%d", task.ID), CallbackData: fmt.Sprintf("t:%d:%d", page, task.ID)},
		}
		if task.PhotoFileID != "" {
			row = append(row, models.InlineKeyboardButton{Text: fmt.Sprintf("📷 #%d", task.ID), CallbackData: fmt.Sprintf("p:%d:%d", page, task.ID)})
		}
		row = append(row, models.InlineKeyboardButton{Text: fmt.Sprintf("🗑 #%d", task.ID), CallbackData: fmt.Sprintf("d:%d:%d", page, task.ID)})
		rows = append(rows, row)
	}
	if pages > 1 {
		var nav []models.InlineKeyboardButton
		if page > 0 {
			nav = append(nav, models.InlineKeyboardButton{Text: "←", CallbackData: fmt.Sprintf("l:%d", page-1)})
		}
		nav = append(nav, models.InlineKeyboardButton{Text: fmt.Sprintf("%d/%d", page+1, pages), CallbackData: fmt.Sprintf("l:%d", page)})
		if page < pages-1 {
			nav = append(nav, models.InlineKeyboardButton{Text: "→", CallbackData: fmt.Sprintf("l:%d", page+1)})
		}
		rows = append(rows, nav)
	}

	return sb.String(), &models.InlineKeyboardMarkup{InlineKeyboard: rows}, nil
}

func (b *Bot) remindersLoop(ctx context.Context) {
	ticker := time.NewTicker(reminderEvery)
	defer ticker.Stop()

	b.sendDueReminders(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			b.sendDueReminders(ctx)
		}
	}
}

func (b *Bot) sendDueReminders(ctx context.Context) {
	tasks, err := b.store.DueReminders(ctx, time.Now())
	if err != nil {
		log.Printf("due reminders: %v", err)
		return
	}

	for _, task := range tasks {
		if err := b.sendReminder(ctx, task); err != nil {
			log.Printf("send reminder #%d: %v", task.ID, err)
			continue
		}
		if err := b.store.MarkReminded(ctx, task.ID, time.Now()); err != nil {
			log.Printf("mark reminded #%d: %v", task.ID, err)
		}
	}
}

func (b *Bot) sendReminder(ctx context.Context, task store.Task) error {
	text := fmt.Sprintf("%s\n#%d %s", reminderMessage, task.ID, task.Text)
	if task.DueAt != nil {
		text += "\nСрок: " + b.fmtDue(*task.DueAt)
	}
	keyboard := &models.InlineKeyboardMarkup{InlineKeyboard: [][]models.InlineKeyboardButton{{
		{Text: "✅ готово", CallbackData: fmt.Sprintf("rt:%d", task.ID)},
		{Text: "🗑 удалить", CallbackData: fmt.Sprintf("rd:%d", task.ID)},
	}}}

	if task.PhotoFileID != "" {
		if err := b.sendPhoto(ctx, task.UserID, task.PhotoFileID, text, keyboard); err == nil {
			return nil
		}
	}
	_, err := b.api.SendMessage(ctx, &telegram.SendMessageParams{
		ChatID:      task.UserID,
		Text:        text,
		ReplyMarkup: keyboard,
	})
	return err
}

func (b *Bot) whenKeyboard(d *draft) *models.InlineKeyboardMarkup {
	now := time.Now().In(b.loc)

	todayEvening := time.Date(now.Year(), now.Month(), now.Day(), 18, 0, 0, 0, b.loc)
	label := "сегодня 18:00"
	if !todayEvening.After(now) {
		todayEvening = now.Add(time.Hour)
		label = "через час"
	}
	tomorrowMorning := time.Date(now.Year(), now.Month(), now.Day(), 9, 0, 0, 0, b.loc).AddDate(0, 0, 1)

	return &models.InlineKeyboardMarkup{InlineKeyboard: [][]models.InlineKeyboardButton{
		{
			{Text: label, CallbackData: fmt.Sprintf("w:%d:%d", d.id, todayEvening.Unix())},
			{Text: "завтра 09:00", CallbackData: fmt.Sprintf("w:%d:%d", d.id, tomorrowMorning.Unix())},
		},
		{
			{Text: "без напоминания", CallbackData: fmt.Sprintf("w:%d:none", d.id)},
			{Text: "другое время", CallbackData: fmt.Sprintf("w:%d:other", d.id)},
		},
	}}
}

func (b *Bot) addedText(task store.Task) string {
	text := fmt.Sprintf("Добавлено #%d: %s", task.ID, task.Text)
	if task.DueAt == nil {
		return text + "\nБез напоминания"
	}
	return text + "\nНапомню " + b.fmtDue(*task.DueAt)
}

func (b *Bot) fmtDue(t time.Time) string {
	t = t.In(b.loc)
	if t.Year() == time.Now().In(b.loc).Year() {
		return t.Format("02.01 15:04")
	}
	return t.Format("02.01.2006 15:04")
}

func (b *Bot) newDraft(userID int64, text, photo string) *draft {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.seq++
	d := &draft{id: b.seq, text: text, photo: photo}
	b.drafts[userID] = d
	return d
}

func (b *Bot) attachPhoto(userID int64, photo string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	d := b.drafts[userID]
	if d == nil {
		return false
	}
	d.photo = photo
	return true
}

func (b *Bot) waitingDraft(userID int64) *draft {
	b.mu.Lock()
	defer b.mu.Unlock()

	d := b.drafts[userID]
	if d == nil || !d.waiting {
		return nil
	}
	copied := *d
	return &copied
}

func (b *Bot) removeDraft(userID, id int64) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if d := b.drafts[userID]; d != nil && d.id == id {
		delete(b.drafts, userID)
	}
}

func (b *Bot) send(ctx context.Context, chatID int64, text string, markup models.ReplyMarkup) {
	if _, err := b.api.SendMessage(ctx, &telegram.SendMessageParams{
		ChatID:      chatID,
		Text:        text,
		ReplyMarkup: markup,
	}); err != nil {
		log.Printf("send message: %v", err)
	}
}

func (b *Bot) sendTask(ctx context.Context, chatID int64, text, photoFileID string, markup models.ReplyMarkup) {
	if photoFileID == "" {
		b.send(ctx, chatID, text, markup)
		return
	}
	if err := b.sendPhoto(ctx, chatID, photoFileID, text, markup); err != nil {
		b.send(ctx, chatID, text, markup)
	}
}

func (b *Bot) sendPhoto(ctx context.Context, chatID int64, fileID, caption string, markup models.ReplyMarkup) error {
	_, err := b.api.SendPhoto(ctx, &telegram.SendPhotoParams{
		ChatID:      chatID,
		Photo:       &models.InputFileString{Data: fileID},
		Caption:     caption,
		ReplyMarkup: markup,
	})
	if err != nil {
		log.Printf("send photo: %v", err)
	}
	return err
}

func (b *Bot) edit(ctx context.Context, chatID int64, messageID int, text string, markup models.ReplyMarkup) {
	if _, err := b.api.EditMessageText(ctx, &telegram.EditMessageTextParams{
		ChatID:      chatID,
		MessageID:   messageID,
		Text:        text,
		ReplyMarkup: markup,
	}); err != nil {
		log.Printf("edit message: %v", err)
	}
}

func (b *Bot) editReminder(ctx context.Context, msg *models.Message, text string) {
	if len(msg.Photo) == 0 {
		b.edit(ctx, msg.Chat.ID, msg.ID, text, noButtons)
		return
	}
	if _, err := b.api.EditMessageCaption(ctx, &telegram.EditMessageCaptionParams{
		ChatID:      msg.Chat.ID,
		MessageID:   msg.ID,
		Caption:     text,
		ReplyMarkup: noButtons,
	}); err != nil {
		log.Printf("edit caption: %v", err)
	}
}

func (b *Bot) answer(ctx context.Context, cq *models.CallbackQuery, text string, alert bool) {
	if _, err := b.api.AnswerCallbackQuery(ctx, &telegram.AnswerCallbackQueryParams{
		CallbackQueryID: cq.ID,
		Text:            text,
		ShowAlert:       alert,
	}); err != nil {
		log.Printf("answer callback: %v", err)
	}
}

func privateMessage(update *models.Update) (*models.Message, bool) {
	if update.Message == nil || update.Message.Chat.Type != models.ChatTypePrivate {
		return nil, false
	}
	return update.Message, true
}

func callbackContext(update *models.Update) (*models.CallbackQuery, *models.Message, bool) {
	cq := update.CallbackQuery
	if cq == nil || cq.Message.Message == nil || cq.Message.Message.Chat.Type != models.ChatTypePrivate {
		return nil, nil, false
	}
	return cq, cq.Message.Message, true
}

func listCallback(data string) (page int, taskID int64, ok bool) {
	parts := strings.SplitN(data, ":", 3)
	if len(parts) != 3 {
		return 0, 0, false
	}
	page, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, 0, false
	}
	taskID, ok = parseInt(parts[2])
	return page, taskID, ok
}

func parseInt(s string) (int64, bool) {
	v, err := strconv.ParseInt(s, 10, 64)
	return v, err == nil
}

func markupOrEmpty(markup models.ReplyMarkup) models.ReplyMarkup {
	if markup == nil {
		return noButtons
	}
	return markup
}
