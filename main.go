package main

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/xuri/excelize/v2"
)

const (
	profileDirName  = ".meli-cancelar-vendas"
	loginCheckURL   = "https://www.mercadolivre.com.br/vendas/omni/lista"
	chatPageURL     = "https://www.mercadolivre.com.br/metricas/meu-atendimento/resumo"
	loginTimeoutMin = 5
)

// deepQueryJS defines helpers that search for elements across the whole page,
// crossing same-origin iframes AND shadow roots. The MELI assistant renders its
// chat inside an <iframe> that is itself nested inside a shadow root
// (host: div#sof-seller-assistant-frm-host), so a plain
// document.querySelectorAll('iframe') no longer reaches the chat input. These
// helpers recurse through shadow roots and iframe documents to find it.
const deepQueryJS = `
(function() {
	if (window.__meliDeepQuery) return;
	window.__meliDeepQuery = function(sel) {
		function search(root) {
			var el = null;
			try { el = root.querySelector(sel); } catch (e) { el = null; }
			if (el) return el;
			var all;
			try { all = root.querySelectorAll('*'); } catch (e) { return null; }
			for (var i = 0; i < all.length; i++) {
				var node = all[i];
				if (node.shadowRoot) { var r = search(node.shadowRoot); if (r) return r; }
				if (node.tagName === 'IFRAME') {
					var d = null;
					try { d = node.contentDocument || (node.contentWindow && node.contentWindow.document); } catch (e) { d = null; }
					if (d) { var r2 = search(d); if (r2) return r2; }
				}
			}
			return null;
		}
		return search(document);
	};
	window.__meliDeepQueryAll = function(sel) {
		var results = [];
		function search(root) {
			var els;
			try { els = root.querySelectorAll(sel); } catch (e) { els = []; }
			for (var i = 0; i < els.length; i++) results.push(els[i]);
			var all;
			try { all = root.querySelectorAll('*'); } catch (e) { return; }
			for (var j = 0; j < all.length; j++) {
				var node = all[j];
				if (node.shadowRoot) search(node.shadowRoot);
				if (node.tagName === 'IFRAME') {
					var d = null;
					try { d = node.contentDocument || (node.contentWindow && node.contentWindow.document); } catch (e) { d = null; }
					if (d) search(d);
				}
			}
		}
		search(document);
		return results;
	};
})();
`

// claimResult holds the parsed result for each claim/sale number.
type claimResult struct {
	Number   string
	Category string // "Excluída", "Sem impacto na reputação", "Continua impactando"
}

func main() {
	log.SetFlags(log.Ltime)

	fmt.Println("╔══════════════════════════════════════════════════╗")
	fmt.Println("║  MELI - Reanálise de reclamações                ║")
	fmt.Println("╚══════════════════════════════════════════════════╝")
	fmt.Println()

	reader := bufio.NewReader(os.Stdin)

	headless := selectMode(reader)
	batchSize := selectBatchSize(reader)

	profileDir := getProfileDir()
	fmt.Printf("[INFO] Perfil do Chrome: %s\n", profileDir)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		fmt.Println("\n[INFO] Encerrando...")
		cancel()
	}()

	chromePath := findChrome()
	if chromePath != "" {
		fmt.Printf("[INFO] Chrome encontrado: %s\n", chromePath)
	}

	opts := buildAllocOpts(profileDir, headless, chromePath)

	if headless {
		fmt.Println("[INFO] Iniciando o Chrome em modo headless...")
	} else {
		fmt.Println("[INFO] Iniciando o Chrome...")
	}

	allocCtx, allocCancel := chromedp.NewExecAllocator(ctx, opts...)
	defer allocCancel()

	browserCtx, browserCancel := chromedp.NewContext(allocCtx,
		chromedp.WithLogf(log.Printf),
	)
	defer browserCancel()

	if err := ensureLogin(browserCtx); err != nil {
		log.Fatalf("[ERRO] Falha ao verificar login: %v", err)
	}

	// Open the chat page once and keep it open
	if err := openChat(browserCtx); err != nil {
		log.Fatalf("[ERRO] Falha ao abrir o chat: %v", err)
	}

	// Persistent line reader for paste-friendly input
	lineCh := make(chan string, 1000)
	go func() {
		for {
			line, err := reader.ReadString('\n')
			lineCh <- strings.TrimSpace(line)
			if err != nil {
				close(lineCh)
				return
			}
		}
	}()

	for {
		inputs := readInputs(lineCh)
		if len(inputs) == 0 {
			fmt.Println("[INFO] Nenhum número fornecido. Encerrando.")
			break
		}

		// Clean and deduplicate inputs
		numbers := cleanInputs(inputs)
		total := len(numbers)
		fmt.Printf("\n[INFO] %d número(s) para processar.\n\n", total)

		if total == 0 {
			fmt.Println("[INFO] Nenhum número válido encontrado.")
			continue
		}

		var allResults []claimResult
		batchCount := (total + batchSize - 1) / batchSize

		for b := 0; b < batchCount; b++ {
			select {
			case <-ctx.Done():
				fmt.Println("[INFO] Cancelado pelo usuário.")
				goto done
			default:
			}

			start := b * batchSize
			end := start + batchSize
			if end > total {
				end = total
			}
			batch := numbers[start:end]

			fmt.Printf("━━━ Lote %d/%d (%d números) ━━━\n", b+1, batchCount, len(batch))

			results, err := processBatch(browserCtx, batch)
			if err != nil {
				fmt.Printf("[ERRO] Falha no lote %d: %v\n", b+1, err)
				// Mark all in this batch as error
				for _, num := range batch {
					allResults = append(allResults, claimResult{Number: num, Category: "Erro ao processar"})
				}
				continue
			}

			allResults = append(allResults, results...)
			fmt.Printf("[OK] Lote %d/%d processado.\n\n", b+1, batchCount)

			// Wait between batches
			if b < batchCount-1 {
				fmt.Println("[INFO] Aguardando 3 segundos antes do próximo lote...")
				time.Sleep(3 * time.Second)
			}
		}

	done:
		printSummary(allResults)
		excelPath := generateExcel(allResults)
		if excelPath != "" {
			fmt.Printf("[INFO] Relatório salvo em: %s\n", excelPath)
		}

		fmt.Println()
		fmt.Println("Cole mais números para continuar ou pressione Enter sem nada para sair.")
	}

	fmt.Println("[INFO] Programa encerrado.")
}

// openChat navigates to the metrics page, clicks the assistant button,
// finds the chat context (iframe/shadow/main), and sets up JS helpers.
// This is called once at startup; the page stays open for all batches.
func openChat(ctx context.Context) error {
	fmt.Println("[INFO] Navegando para a página de métricas...")
	if err := chromedp.Run(ctx,
		chromedp.Navigate(chatPageURL),
		chromedp.WaitReady("body", chromedp.ByQuery),
	); err != nil {
		return fmt.Errorf("navegar para métricas: %w", err)
	}
	time.Sleep(5 * time.Second)

	// Click the assistant floating button
	fmt.Println("[INFO] Abrindo o assistente...")
	for attempt := 0; attempt < 3; attempt++ {
		chromedp.Run(ctx, chromedp.Evaluate(`
			(function() {
				var selectors = [
					'button.action-button[data-component="WIDGET"]',
					'button[aria-label="Perguntar ao assistente"]',
					'button[aria-label*="assistente"]',
					'button[aria-label*="Assistente"]',
				];
				for (var s = 0; s < selectors.length; s++) {
					var btn = document.querySelector(selectors[s]);
					if (btn) { btn.click(); return true; }
				}
				var all = document.querySelectorAll('button, span, a');
				for (var i = 0; i < all.length; i++) {
					var text = all[i].textContent.trim();
					if (text === 'Assistente' || text.includes('Assistente')) {
						all[i].click();
						return true;
					}
				}
				return false;
			})()
		`, nil))
		time.Sleep(3 * time.Second)

		var found bool
		chromedp.Run(ctx, chromedp.Evaluate(deepQueryJS+`
			(function() {
				return !!(window.__meliDeepQuery('#chat-input') || window.__meliDeepQuery('.chat-messages'));
			})()
		`, &found))
		if found {
			break
		}
	}

	// Find the chat input, crossing iframes and shadow DOM.
	fmt.Println("[INFO] Verificando se o chat está aberto...")
	var chatFound bool
	for attempt := 0; attempt < 20; attempt++ {
		chromedp.Run(ctx, chromedp.Evaluate(deepQueryJS+`
			(function() {
				var input = window.__meliDeepQuery('#chat-input') ||
							window.__meliDeepQuery('textarea[placeholder*="Pergunte"]') ||
							window.__meliDeepQuery('textarea[aria-label*="mensagem"]');
				return !!input;
			})()
		`, &chatFound))

		if chatFound {
			fmt.Println("[INFO] Chat encontrado (input localizado).")
			break
		}
		if attempt%5 == 4 {
			fmt.Printf("  Tentativa %d - chat ainda não encontrado, tentando abrir novamente...\n", attempt+1)
			chromedp.Run(ctx, chromedp.Evaluate(`
				(function() {
					var all = document.querySelectorAll('button, span, a');
					for (var i = 0; i < all.length; i++) {
						if (all[i].textContent.trim().includes('Assistente')) {
							all[i].click(); return true;
						}
					}
					return false;
				})()
			`, nil))
		}
		time.Sleep(1 * time.Second)
	}

	if !chatFound {
		return fmt.Errorf("chat não abriu - input não encontrado")
	}

	// Pre-inject the deep-query helpers so all later steps can reuse them.
	chromedp.Run(ctx, chromedp.Evaluate(deepQueryJS+`true`, nil))

	fmt.Println("[OK] Chat aberto e pronto para uso.")
	return nil
}

// startNewConversation clicks the "Editar" (new conversation) button,
// waits 5s, checks if chat reset. If not, clicks again. Repeats up to 10 times.
func startNewConversation(ctx context.Context) error {
	fmt.Println("  Iniciando nova conversa...")

	for attempt := 0; attempt < 10; attempt++ {
		// Click the edit/new conversation button
		chromedp.Run(ctx, chromedp.Evaluate(deepQueryJS+`
			(function() {
				var btn = window.__meliDeepQuery('#sa-icon-edit-chat');
				if (btn) { btn.click(); return 'clicked-edit'; }
				var btns = window.__meliDeepQueryAll('button');
				for (var i = 0; i < btns.length; i++) {
					var label = btns[i].getAttribute('aria-label') || '';
					if (label === 'Editar' || label.includes('nova') || label.includes('Nova conversa')) {
						btns[i].click();
						return 'clicked-' + label;
					}
				}
				btn = window.__meliDeepQuery('.assistant-chat-header__toolbar-action--edit');
				if (btn) { btn.click(); return 'clicked-class'; }
				return 'not-found';
			})()
		`, nil))

		time.Sleep(5 * time.Second)

		// Check if chat reset
		var ready bool
		chromedp.Run(ctx, chromedp.Evaluate(deepQueryJS+`
			(function() {
				var msgs = window.__meliDeepQueryAll('.message-item--assistant');
				if (msgs.length === 0) return true;
				if (msgs.length === 1) {
					var text = msgs[0].textContent || '';
					if (text.includes('Olá') || text.includes('Como posso') || text.includes('ajudar')) return true;
				}
				var userMsgs = window.__meliDeepQueryAll('.message-item--user');
				if (userMsgs.length === 0) return true;
				return false;
			})()
		`, &ready))

		if ready {
			fmt.Println("  Nova conversa iniciada com sucesso.")
			return nil
		}
		fmt.Printf("  Chat não resetou, enviando 'teste' e tentando novamente... (tentativa %d)\n", attempt+2)
		// Send "teste" twice, waiting for chat response each time
		countBefore := getAssistantMsgCount(ctx)
		sendChatMessage(ctx, "teste")
		waitForResponse(ctx, countBefore, 30*time.Second)

		countBefore = getAssistantMsgCount(ctx)
		sendChatMessage(ctx, "teste")
		waitForResponse(ctx, countBefore, 30*time.Second)
	}

	return fmt.Errorf("não conseguiu iniciar nova conversa após 10 tentativas")
}

// processBatch handles a single batch of numbers via the MELI chat.
// Assumes the chat is already open (openChat was called once).
func processBatch(ctx context.Context, numbers []string) ([]claimResult, error) {
	// Step 1: Start a new conversation
	if err := startNewConversation(ctx); err != nil {
		return nil, fmt.Errorf("nova conversa: %w", err)
	}

	// Step 2: Send the initial prompt
	prompt := "Remova as reclamações abaixo que estão impactando minha reputação e podem ser excluídas:"
	fmt.Println("  Enviando prompt...")
	countBeforePrompt := getAssistantMsgCount(ctx)
	if err := sendChatMessage(ctx, prompt); err != nil {
		return nil, fmt.Errorf("enviar prompt: %w", err)
	}

	// Wait for assistant response
	fmt.Println("  Aguardando resposta do assistente...")
	if err := waitForResponse(ctx, countBeforePrompt, 30*time.Second); err != nil {
		return nil, fmt.Errorf("aguardar resposta: %w", err)
	}

	// Step 3: Send the numbers
	numbersText := strings.Join(numbers, "\n")
	fmt.Printf("  Enviando %d números...\n", len(numbers))
	countBeforeNumbers := getAssistantMsgCount(ctx)
	if err := sendChatMessage(ctx, numbersText); err != nil {
		return nil, fmt.Errorf("enviar números: %w", err)
	}

	// Step 4: Wait for the analysis response
	fmt.Println("  Aguardando análise do MELI (timeout: 30 minutos)...")
	if err := waitForResponse(ctx, countBeforeNumbers, 30*time.Minute); err != nil {
		return nil, fmt.Errorf("aguardar análise: %w", err)
	}

	// Step 5: Extract the response
	fmt.Println("  Coletando resposta...")
	responseText, err := getLastAssistantMessage(ctx)
	if err != nil {
		return nil, fmt.Errorf("coletar resposta: %w", err)
	}

	fmt.Printf("  Resposta do MELI:\n")
	for _, line := range strings.Split(responseText, "\n") {
		fmt.Printf("    %s\n", line)
	}
	fmt.Println()

	// Step 6: Parse the response
	results := parseResponse(responseText, numbers)
	return results, nil
}

// sendChatMessage types a message into the chat input and sends it.
// Uses the iframe's own window for React value setters and events.
func sendChatMessage(ctx context.Context, message string) error {
	var result string
	if err := chromedp.Run(ctx, chromedp.Evaluate(deepQueryJS+fmt.Sprintf(`
		(function() {
			var input = window.__meliDeepQuery('#chat-input') ||
						window.__meliDeepQuery('textarea[placeholder*="Pergunte"]') ||
						window.__meliDeepQuery('textarea[aria-label*="mensagem"]') ||
						window.__meliDeepQuery('textarea[aria-label*="chat"]');
			if (!input) return 'no-input';

			// Resolve the window that owns the input (iframe window for React setter).
			var win = input.ownerDocument.defaultView || window;
			input.focus();
			// Use the owning window's prototype setter (critical for React)
			var setter = Object.getOwnPropertyDescriptor(win.HTMLTextAreaElement.prototype, 'value').set;
			setter.call(input, %q);
			// Dispatch events using the owning window's Event constructor
			input.dispatchEvent(new win.Event('input', { bubbles: true }));
			input.style.height = 'auto';
			input.style.height = input.scrollHeight + 'px';
			return 'ok';
		})()
	`, message), &result)); err != nil {
		return fmt.Errorf("definir texto: %w", err)
	}
	if result != "ok" {
		return fmt.Errorf("falha ao definir texto: %s", result)
	}

	time.Sleep(500 * time.Millisecond)

	// Send: try clicking send button, then fall back to Enter key
	chromedp.Run(ctx, chromedp.Evaluate(deepQueryJS+`
		(function() {
			var input = window.__meliDeepQuery('#chat-input') ||
						window.__meliDeepQuery('textarea[placeholder*="Pergunte"]');
			if (!input) return 'no-input';
			var win = input.ownerDocument.defaultView || window;

			// Look for a send button anywhere (crossing iframe/shadow)
			var btns = window.__meliDeepQueryAll('button');
			for (var i = 0; i < btns.length; i++) {
				var label = btns[i].getAttribute('aria-label') || '';
				if (label.includes('nviar') || label.includes('Enviar') || label.includes('Send') || label.includes('end message')) {
					if (!btns[i].disabled) { btns[i].click(); return 'sent-button'; }
				}
			}
			// Fallback: Enter key on the input
			input.dispatchEvent(new win.KeyboardEvent('keydown', {key: 'Enter', code: 'Enter', keyCode: 13, which: 13, bubbles: true}));
			return 'sent-enter';
		})()
	`, nil))

	time.Sleep(1 * time.Second)
	return nil
}

// getAssistantMsgCount returns the current count of assistant messages in the chat.
func getAssistantMsgCount(ctx context.Context) int {
	var count int
	chromedp.Run(ctx, chromedp.Evaluate(deepQueryJS+`
		(function() {
			return window.__meliDeepQueryAll('.message-item--assistant').length;
		})()
	`, &count))
	return count
}

// waitForResponse waits until a NEW assistant message appears and stabilizes.
// initialCount is the number of assistant messages before the user message was sent.
func waitForResponse(ctx context.Context, initialCount int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	time.Sleep(3 * time.Second)

	prevText := ""
	stableCount := 0
	requiredStable := 5

	for time.Now().Before(deadline) {
		currentCount := getAssistantMsgCount(ctx)

		if currentCount > initialCount {
			// New message appeared — read its text
			var currentText string
			chromedp.Run(ctx, chromedp.Evaluate(deepQueryJS+`
				(function() {
					var msgs = window.__meliDeepQueryAll('.message-item--assistant');
					if (msgs.length === 0) return '';
					var last = msgs[msgs.length - 1];
					var content = last.querySelector('.chat-message__content');
					return content ? content.textContent.trim() : last.textContent.trim();
				})()
			`, &currentText))

			if currentText != "" && currentText == prevText {
				stableCount++
				if stableCount >= requiredStable {
					return nil // text stabilized
				}
			} else {
				stableCount = 0
				prevText = currentText
			}
		}

		time.Sleep(3 * time.Second)
	}

	// If we got some text despite timeout, consider it done
	if prevText != "" {
		return nil
	}
	return fmt.Errorf("timeout aguardando resposta do assistente")
}

// getLastAssistantMessage retrieves the text of the last assistant message.
func getLastAssistantMessage(ctx context.Context) (string, error) {
	var text string
	err := chromedp.Run(ctx, chromedp.Evaluate(deepQueryJS+`
		(function() {
			var msgs = window.__meliDeepQueryAll('.message-item--assistant');
			if (msgs.length === 0) return '';
			var last = msgs[msgs.length - 1];
			var content = last.querySelector('.chat-message__content');
			if (content) return content.textContent.trim();
			return last.textContent.trim();
		})()
	`, &text))
	if err != nil {
		return "", err
	}
	if text == "" {
		return "", fmt.Errorf("resposta vazia do assistente")
	}
	return text, nil
}

// categorizeAfterNumber looks at text immediately following a number for per-number format.
// e.g., "5503663801: sem impacto" → "no_impact"
func categorizeAfterNumber(textAfter string) string {
	t := strings.ToLower(strings.TrimSpace(textAfter))
	// Match patterns like ": sem impacto", ": excluída", ": continua impactando"
	if strings.HasPrefix(t, ":") || strings.HasPrefix(t, " ") {
		t = strings.TrimLeft(t, ": ")
	}
	if strings.HasPrefix(t, "sem impacto") || strings.HasPrefix(t, "não afeta") ||
		strings.HasPrefix(t, "não impacta") || strings.HasPrefix(t, "já sem impacto") {
		return "no_impact"
	}
	if strings.HasPrefix(t, "excluíd") || strings.HasPrefix(t, "excluid") ||
		strings.HasPrefix(t, "xcluíd") || strings.HasPrefix(t, "removid") {
		return "excluded"
	}
	if strings.HasPrefix(t, "continua") || strings.HasPrefix(t, "impactando") ||
		strings.HasPrefix(t, "não consegu") || strings.HasPrefix(t, "não foi") {
		return "impacting"
	}
	return ""
}

// parseResponse parses the assistant's response text and categorizes each number.
// Handles two formats:
// 1. Per-number: "5503663801: sem impacto" — text after each number indicates its category
// 2. Section-based: "Sem impacto agora5503663801..." — numbers grouped under section headers
func parseResponse(response string, originalNumbers []string) []claimResult {
	responseLower := strings.ToLower(response)
	numberCategories := make(map[string]string)

	// --- Strategy 1: Per-number format ("number: category") ---
	for _, num := range originalNumbers {
		pos := strings.Index(responseLower, num)
		if pos < 0 {
			continue
		}
		afterPos := pos + len(num)
		if afterPos < len(responseLower) {
			// Read up to 60 chars after the number
			end := afterPos + 60
			if end > len(responseLower) {
				end = len(responseLower)
			}
			textAfter := responseLower[afterPos:end]
			cat := categorizeAfterNumber(textAfter)
			if cat != "" {
				numberCategories[num] = cat
			}
		}
	}

	// If per-number found results for most numbers, use that
	if len(numberCategories) >= len(originalNumbers)/2 && len(numberCategories) > 0 {
		// per-number format detected
	} else {
		// --- Strategy 2: Section-based format ---
		numberCategories = make(map[string]string) // reset

		type sectionMarker struct {
			pos      int
			category string
		}
		var markers []sectionMarker

		sectionKeywords := map[string]string{
			"sem impacto agora":    "no_impact",
			"sem impacto na":       "no_impact",
			"não afetam":           "no_impact",
			"não impactam":         "no_impact",
			"não afeta":            "no_impact",
			"continua impactando":  "impacting",
			"continuam impactando": "impacting",
			"seguem impactando":    "impacting",
			"segue impactando":     "impacting",
			"não consegui":         "impacting",
			"não foi possível":     "impacting",
			"excluída agora":       "excluded",
			"excluídas agora":      "excluded",
			"excluído agora":       "excluded",
			"excluídos agora":      "excluded",
			"removida agora":       "excluded",
			"removidas agora":      "excluded",
			"removido agora":       "excluded",
			"removidos agora":      "excluded",
			"foram excluíd":        "excluded",
			"foram removid":        "excluded",
			"foi excluíd":          "excluded",
			"foi removid":          "excluded",
			"xcluída agora":        "excluded",
			"xcluídas agora":       "excluded",
		}

		for keyword, cat := range sectionKeywords {
			idx := 0
			for {
				pos := strings.Index(responseLower[idx:], keyword)
				if pos < 0 {
					break
				}
				markers = append(markers, sectionMarker{pos: idx + pos, category: cat})
				idx += pos + len(keyword)
			}
		}

		// Sort markers by position
		for i := 0; i < len(markers); i++ {
			for j := i + 1; j < len(markers); j++ {
				if markers[j].pos < markers[i].pos {
					markers[i], markers[j] = markers[j], markers[i]
				}
			}
		}

		for _, num := range originalNumbers {
			pos := strings.Index(responseLower, num)
			if pos < 0 {
				continue
			}
			category := ""
			for _, m := range markers {
				if m.pos < pos {
					category = m.category
				}
			}
			if category != "" {
				numberCategories[num] = category
			}
		}
	}

	// Build results
	results := make([]claimResult, 0, len(originalNumbers))
	for _, num := range originalNumbers {
		cat, found := numberCategories[num]
		label := "Não identificado na resposta"
		if found {
			switch cat {
			case "excluded":
				label = "Excluída"
			case "no_impact":
				label = "Sem impacto na reputação"
			case "impacting":
				label = "Continua impactando"
			}
		} else if strings.Contains(responseLower, "nenhum") && (strings.Contains(responseLower, "aprovad") || strings.Contains(responseLower, "remoção")) {
			label = "Continua impactando"
		}
		results = append(results, claimResult{Number: num, Category: label})
	}

	return results
}

// --- Input handling ---

func readInputs(lineCh <-chan string) []string {
	fmt.Println("Cole os números de venda ou reclamação abaixo.")
	fmt.Println("Após colar, aguarde 2 segundos para iniciar automaticamente,")
	fmt.Println("ou pressione Enter em uma linha vazia para iniciar.")
	fmt.Println()

	var inputs []string
	for {
		if len(inputs) == 0 {
			line, ok := <-lineCh
			if !ok {
				return inputs
			}
			if line == "" {
				return nil
			}
			inputs = append(inputs, line)
			fmt.Printf("  + Entrada adicionada (%d)\n", len(inputs))
		} else {
			select {
			case line, ok := <-lineCh:
				if !ok || line == "" {
					return inputs
				}
				inputs = append(inputs, line)
				fmt.Printf("  + Entrada adicionada (%d)\n", len(inputs))
			case <-time.After(2 * time.Second):
				fmt.Printf("  [AUTO] %d entrada(s) detectada(s). Iniciando...\n", len(inputs))
				return inputs
			}
		}
	}
}

func cleanInputs(inputs []string) []string {
	re := regexp.MustCompile(`\d{7,20}`)
	seen := make(map[string]bool)
	var result []string
	for _, input := range inputs {
		// Extract numbers from each line
		matches := re.FindAllString(input, -1)
		for _, m := range matches {
			if !seen[m] {
				seen[m] = true
				result = append(result, m)
			}
		}
	}
	return result
}

// --- Chrome setup ---

func buildAllocOpts(profileDir string, headless bool, chromePath string) []chromedp.ExecAllocatorOption {
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.UserDataDir(profileDir),
		chromedp.Flag("disable-gpu", false),
		chromedp.Flag("no-first-run", true),
		chromedp.Flag("no-default-browser-check", true),
		chromedp.Flag("disable-extensions", false),
		chromedp.Flag("no-sandbox", true),
		chromedp.WindowSize(1280, 900),
	)
	if headless {
		opts = append(opts, chromedp.Flag("headless", "new"))
		opts = append(opts, chromedp.Flag("disable-blink-features", "AutomationControlled"))
		opts = append(opts, chromedp.UserAgent("Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/133.0.0.0 Safari/537.36"))
	} else {
		opts = append(opts, chromedp.Flag("headless", false))
	}
	if chromePath != "" {
		opts = append(opts, chromedp.ExecPath(chromePath))
	}
	return opts
}

func selectMode(reader *bufio.Reader) bool {
	fmt.Println("Selecione o modo de execução:")
	fmt.Println("  1 - Chrome visível (mais consumo de memória e CPU)")
	fmt.Println("  2 - Chrome em background (mais performance)")
	fmt.Println()

	for {
		fmt.Print("Opção (1 ou 2): ")
		line, _ := reader.ReadString('\n')
		choice := strings.TrimSpace(line)
		switch choice {
		case "1":
			fmt.Println("[INFO] Modo: Chrome visível")
			fmt.Println()
			return false
		case "2":
			fmt.Println("[INFO] Modo: Chrome em background")
			fmt.Println()
			return true
		default:
			fmt.Println("  Opção inválida. Digite 1 ou 2.")
		}
	}
}

func selectBatchSize(reader *bufio.Reader) int {
	fmt.Println("Quantos números enviar por lote? (1 a 100, padrão: 50)")
	fmt.Print("Quantidade: ")
	line, _ := reader.ReadString('\n')
	choice := strings.TrimSpace(line)
	if choice == "" {
		fmt.Println("[INFO] Usando padrão: 50 por lote")
		fmt.Println()
		return 50
	}
	n, err := strconv.Atoi(choice)
	if err != nil || n < 1 || n > 100 {
		fmt.Println("  Valor inválido. Usando padrão: 50 por lote")
		fmt.Println()
		return 50
	}
	fmt.Printf("[INFO] Lote: %d números por vez\n", n)
	fmt.Println()
	return n
}

func getProfileDir() string {
	var base string
	switch runtime.GOOS {
	case "windows":
		base = os.Getenv("LOCALAPPDATA")
		if base == "" {
			base = os.Getenv("USERPROFILE")
		}
	case "darwin":
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, "Library", "Application Support")
	default:
		home, _ := os.UserHomeDir()
		base = home
	}
	return filepath.Join(base, profileDirName)
}

func ensureLogin(ctx context.Context) error {
	fmt.Println("[INFO] Verificando login no Mercado Livre...")

	if err := chromedp.Run(ctx,
		chromedp.Navigate(loginCheckURL),
		chromedp.WaitReady("body", chromedp.ByQuery),
	); err != nil {
		return fmt.Errorf("navegar para ML: %w", err)
	}

	time.Sleep(3 * time.Second)

	var currentURL string
	if err := chromedp.Run(ctx, chromedp.Location(&currentURL)); err != nil {
		return fmt.Errorf("obter URL atual: %w", err)
	}

	if strings.Contains(currentURL, "login") || strings.Contains(currentURL, "registration") {
		fmt.Println()
		fmt.Println("╔══════════════════════════════════════════════════╗")
		fmt.Println("║  LOGIN NECESSÁRIO                                ║")
		fmt.Println("║                                                  ║")
		fmt.Println("║  O Chrome abriu a página de login do ML.         ║")
		fmt.Println("║  Faça login na janela do Chrome que abriu.       ║")
		fmt.Printf("║  Aguardando até %d minutos...                    ║\n", loginTimeoutMin)
		fmt.Println("╚══════════════════════════════════════════════════╝")
		fmt.Println()

		timeout := time.After(time.Duration(loginTimeoutMin) * time.Minute)
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-timeout:
				return fmt.Errorf("timeout: login não realizado em %d minutos", loginTimeoutMin)
			case <-ticker.C:
				if err := chromedp.Run(ctx, chromedp.Location(&currentURL)); err != nil {
					continue
				}
				if !strings.Contains(currentURL, "login") && !strings.Contains(currentURL, "registration") {
					fmt.Println("[OK] Login detectado com sucesso!")
					time.Sleep(2 * time.Second)
					return nil
				}
			}
		}
	}

	fmt.Println("[OK] Já está logado no Mercado Livre!")
	return nil
}

func findChrome() string {
	candidates := []string{
		"/usr/bin/google-chrome-stable",
		"/usr/bin/google-chrome",
		"/usr/bin/chromium-browser",
		"/usr/bin/chromium",
		"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
		"/opt/.devin/chrome/chrome/linux-133.0.6943.126/chrome-linux64/chrome",
		"/opt/.devin/chrome/chrome/linux-137.0.7118.2/chrome-linux64/chrome",
	}

	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return ""
}

// --- Summary & Excel ---

func printSummary(results []claimResult) {
	excluded := 0
	noImpact := 0
	impacting := 0
	errors := 0
	for _, r := range results {
		switch r.Category {
		case "Excluída":
			excluded++
		case "Sem impacto na reputação":
			noImpact++
		case "Continua impactando":
			impacting++
		default:
			errors++
		}
	}

	fmt.Println()
	fmt.Println("═══════════════════════════════════════")
	fmt.Printf("Total analisado: %d\n", len(results))
	fmt.Printf("Excluídas: %d\n", excluded)
	fmt.Printf("Continuam impactando: %d\n", impacting)
	fmt.Printf("Sem impacto na reputação: %d\n", noImpact)
	if errors > 0 {
		fmt.Printf("Não identificados: %d\n", errors)
	}
	fmt.Println("═══════════════════════════════════════")
}

func generateExcel(results []claimResult) string {
	if len(results) == 0 {
		return ""
	}

	f := excelize.NewFile()
	sheet := "Reanálise"
	f.SetSheetName("Sheet1", sheet)

	// Summary counts
	excluded := 0
	noImpact := 0
	impacting := 0
	for _, r := range results {
		switch r.Category {
		case "Excluída":
			excluded++
		case "Sem impacto na reputação":
			noImpact++
		case "Continua impactando":
			impacting++
		}
	}

	// Header style
	headerStyle, _ := f.NewStyle(&excelize.Style{
		Font:      &excelize.Font{Bold: true, Color: "FFFFFF", Size: 11},
		Fill:      excelize.Fill{Type: "pattern", Color: []string{"2D6AB1"}, Pattern: 1},
		Alignment: &excelize.Alignment{Horizontal: "center", Vertical: "center"},
		Border: []excelize.Border{
			{Type: "left", Color: "000000", Style: 1},
			{Type: "right", Color: "000000", Style: 1},
			{Type: "top", Color: "000000", Style: 1},
			{Type: "bottom", Color: "000000", Style: 1},
		},
	})

	// Summary style
	summaryStyle, _ := f.NewStyle(&excelize.Style{
		Font:      &excelize.Font{Bold: true, Size: 11},
		Alignment: &excelize.Alignment{Horizontal: "left"},
	})

	// Data styles by category
	excludedStyle, _ := f.NewStyle(&excelize.Style{
		Font: &excelize.Font{Color: "008000"},
		Fill: excelize.Fill{Type: "pattern", Color: []string{"E8F5E9"}, Pattern: 1},
		Border: []excelize.Border{
			{Type: "left", Color: "000000", Style: 1},
			{Type: "right", Color: "000000", Style: 1},
			{Type: "top", Color: "000000", Style: 1},
			{Type: "bottom", Color: "000000", Style: 1},
		},
	})

	impactingStyle, _ := f.NewStyle(&excelize.Style{
		Font: &excelize.Font{Color: "CC0000"},
		Fill: excelize.Fill{Type: "pattern", Color: []string{"FFEBEE"}, Pattern: 1},
		Border: []excelize.Border{
			{Type: "left", Color: "000000", Style: 1},
			{Type: "right", Color: "000000", Style: 1},
			{Type: "top", Color: "000000", Style: 1},
			{Type: "bottom", Color: "000000", Style: 1},
		},
	})

	noImpactStyle, _ := f.NewStyle(&excelize.Style{
		Font: &excelize.Font{Color: "666666"},
		Fill: excelize.Fill{Type: "pattern", Color: []string{"F5F5F5"}, Pattern: 1},
		Border: []excelize.Border{
			{Type: "left", Color: "000000", Style: 1},
			{Type: "right", Color: "000000", Style: 1},
			{Type: "top", Color: "000000", Style: 1},
			{Type: "bottom", Color: "000000", Style: 1},
		},
	})

	// Summary section
	f.SetCellValue(sheet, "A1", "Resumo da Reanálise")
	f.SetCellStyle(sheet, "A1", "A1", summaryStyle)
	f.SetCellValue(sheet, "A2", "Total analisado:")
	f.SetCellValue(sheet, "B2", len(results))
	f.SetCellValue(sheet, "A3", "Excluídas:")
	f.SetCellValue(sheet, "B3", excluded)
	f.SetCellValue(sheet, "A4", "Continuam impactando:")
	f.SetCellValue(sheet, "B4", impacting)
	f.SetCellValue(sheet, "A5", "Sem impacto na reputação:")
	f.SetCellValue(sheet, "B5", noImpact)

	// Data headers
	row := 7
	headers := []string{"Número da operação", "Resultado"}
	for i, h := range headers {
		cell := fmt.Sprintf("%c%d", 'A'+i, row)
		f.SetCellValue(sheet, cell, h)
		f.SetCellStyle(sheet, cell, cell, headerStyle)
	}

	// Data rows
	for i, r := range results {
		dataRow := row + 1 + i
		cellA := fmt.Sprintf("A%d", dataRow)
		cellB := fmt.Sprintf("B%d", dataRow)
		f.SetCellValue(sheet, cellA, r.Number)
		f.SetCellValue(sheet, cellB, r.Category)

		// Apply style based on category
		switch r.Category {
		case "Excluída":
			f.SetCellStyle(sheet, cellA, cellB, excludedStyle)
		case "Continua impactando":
			f.SetCellStyle(sheet, cellA, cellB, impactingStyle)
		case "Sem impacto na reputação":
			f.SetCellStyle(sheet, cellA, cellB, noImpactStyle)
		}
	}

	// Adjust column widths
	f.SetColWidth(sheet, "A", "A", 25)
	f.SetColWidth(sheet, "B", "B", 35)

	// Save to executable directory
	exePath, err := os.Executable()
	if err != nil {
		exePath = "."
	}
	exeDir := filepath.Dir(exePath)

	now := time.Now()
	name := fmt.Sprintf("reanalise_reclamacao_%s.xlsx", now.Format("2006-01-02_15-04-05"))
	fullPath := filepath.Join(exeDir, name)

	if err := f.SaveAs(fullPath); err != nil {
		// Fallback: save in current directory
		fullPath = name
		if err := f.SaveAs(fullPath); err != nil {
			fmt.Printf("[ERRO] Falha ao salvar Excel: %v\n", err)
			return ""
		}
	}

	return fullPath
}
