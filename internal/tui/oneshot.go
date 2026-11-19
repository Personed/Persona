package tui

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/Personed/Persona/internal/client"
)

// Chat runs persona chat: send one message and stream the reply, or, without a message,
// print the history and read messages line by line from stdin.
func Chat(ctx context.Context, args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("persona chat", flag.ContinueOnError)
	flags.SetOutput(stdout)
	server := flags.String("server", "", "Persona service URL (default as persona tui)")
	botID := flags.Int("bot", 0, "Bot ID (default the first Bot)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	c, _, err := connect(*server)
	if err != nil {
		return err
	}
	var bots []client.Bot
	if err := c.Call(ctx, "bots.list", nil, &bots); err != nil {
		if unauthorized(err) {
			return errors.New("this terminal is not paired: run persona tui to pair it")
		}
		return err
	}
	bot, err := pickBot(bots, *botID)
	if err != nil {
		return err
	}
	if text := strings.TrimSpace(strings.Join(flags.Args(), " ")); text != "" {
		return chatOnce(ctx, c, bot, text, stdout)
	}
	var view client.ChatView
	if err := c.Call(ctx, "get", map[string]int{"botId": bot.ID}, &view); err != nil {
		return err
	}
	for _, m := range view.Messages {
		if text := m.Text(); text != "" && (m.Role == "user" || m.Role == "assistant") {
			fmt.Fprintf(stdout, "%s> %s\n", speaker(m.Role, bot), text)
		}
	}
	fmt.Fprintln(stdout, "Type a message. Ctrl+D or Ctrl+C to quit.")
	lines := bufio.NewScanner(os.Stdin)
	for fmt.Fprint(stdout, "You> "); lines.Scan(); fmt.Fprint(stdout, "You> ") {
		if text := strings.TrimSpace(lines.Text()); text != "" {
			if err := chatOnce(ctx, c, bot, text, stdout); err != nil {
				return err
			}
		}
	}
	fmt.Fprintln(stdout)
	return lines.Err()
}

func pickBot(bots []client.Bot, id int) (client.Bot, error) {
	for _, b := range bots {
		if id == 0 || b.ID == id {
			return b, nil
		}
	}
	if id == 0 {
		return client.Bot{}, errors.New("the service has no Bots")
	}
	return client.Bot{}, fmt.Errorf("no Bot with ID %d", id)
}

func speaker(role string, bot client.Bot) string {
	if role == "user" {
		return "You"
	}
	return bot.Name
}

// chatOnce sends text and prints the new assistant text as it streams, until the Bot is idle again.
func chatOnce(ctx context.Context, c *client.Client, bot client.Bot, text string, out io.Writer) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream, err := c.Subscribe(ctx, "changes", map[string]int{"botId": bot.ID})
	if err != nil {
		return err
	}
	defer stream.Close()
	var view client.ChatView
	var change client.Change
	if err := stream.Next(&change); err != nil {
		return err
	}
	if err := view.Apply(change); err != nil {
		return err
	}
	known := map[string]bool{}
	for _, m := range view.Messages {
		known[m.ID] = true
	}
	if err := c.Call(ctx, "send", map[string]any{"botId": bot.ID, "text": text, "requestId": requestID()}, nil); err != nil {
		return err
	}
	fmt.Fprintf(out, "%s> ", bot.Name)
	printed := map[string]string{}
	replied, waiting := false, false
	for {
		if err := stream.Next(&change); err != nil {
			fmt.Fprintln(out)
			if errors.Is(err, io.EOF) {
				return errors.New("the service closed the stream before the reply finished")
			}
			return err
		}
		if err := view.Apply(change); err != nil {
			return err
		}
		for _, m := range view.Messages {
			if known[m.ID] || m.Role != "assistant" {
				continue
			}
			replied = true
			now := m.Text()
			if before := printed[m.ID]; strings.HasPrefix(now, before) {
				fmt.Fprint(out, now[len(before):])
			} else {
				fmt.Fprint(out, "\n"+now)
			}
			printed[m.ID] = now
		}
		if len(view.Approvals) > 0 && !waiting {
			waiting = true
			fmt.Fprintf(out, "\n[waiting: %s - answer it in persona tui]\n", view.Approvals[0].Title)
		}
		if view.Failed || view.Error != "" {
			fmt.Fprintln(out)
			return fmt.Errorf("the model did not finish the reply: %s", view.Error)
		}
		if replied && !view.Busy && !view.Processing {
			fmt.Fprintln(out)
			return nil
		}
	}
}
