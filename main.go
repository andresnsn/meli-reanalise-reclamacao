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
	batchSize       = 50
)

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

		// Process in batches of 50
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

			results, err := processBatch(browserCtx, batch, b == 0)
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

// processBatch handles a single batch of up to 50 numbers via the MELI chat.
// The chat widget is rendered inside an iframe, so all JS queries use a helper
// function that searches both the main document and any iframes/shadow roots.
func processBatch(ctx context.Context, numbers []string, isFirstBatch bool) ([]claimResult, error) {
	// Step 1: Navigate to the chat page
	fmt.Println("  Navegando para a página de métricas...")
	if err := chromedp.Run(ctx,
		chromedp.Navigate(chatPageURL),
		chromedp.WaitReady("body", chromedp.ByQuery),
	); err != nil {
		return nil, fmt.Errorf("navegar para métricas: %w", err)
	}
	time.Sleep(5 * time.Second)

	// Step 2: Open the assistant chat by clicking the floating button
	fmt.Println("  Abrindo o assistente...")
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

		// Check if chat opened (look for iframe or chat elements)
		var found bool
		chromedp.Run(ctx, chromedp.Evaluate(`
			(function() {
				// Chat might be in an iframe
				var iframes = document.querySelectorAll('iframe');
				for (var i = 0; i < iframes.length; i++) {
					try {
						var doc = iframes[i].contentDocument || iframes[i].contentWindow.document;
						if (doc && doc.querySelector('#chat-input, .chat-messages')) return true;
					} catch(e) {}
				}
				// Or directly in the DOM
				return document.querySelector('#chat-input, .chat-messages') !== null;
			})()
		`, &found))
		if found {
			break
		}
	}

	// Step 3: Find the chat input - search in main document, iframes, and shadow roots
	fmt.Println("  Verificando se o chat está aberto...")
	var chatFound bool
	var chatLocation string // "main", "iframe", "shadow"
	for attempt := 0; attempt < 20; attempt++ {
		chromedp.Run(ctx, chromedp.Evaluate(`
			(function() {
				// Check main document
				if (document.querySelector('#chat-input')) return 'main';
				// Check iframes
				var iframes = document.querySelectorAll('iframe');
				for (var i = 0; i < iframes.length; i++) {
					try {
						var doc = iframes[i].contentDocument || iframes[i].contentWindow.document;
						if (doc && doc.querySelector('#chat-input')) return 'iframe-' + i;
						// Also check for textarea with placeholder
						if (doc && doc.querySelector('textarea[placeholder*="Pergunte"]')) return 'iframe-' + i;
					} catch(e) {
						// Cross-origin iframe - can't access
					}
				}
				// Check shadow roots
				var all = document.querySelectorAll('*');
				for (var i = 0; i < all.length; i++) {
					if (all[i].shadowRoot) {
						if (all[i].shadowRoot.querySelector('#chat-input')) return 'shadow';
						if (all[i].shadowRoot.querySelector('textarea[placeholder*="Pergunte"]')) return 'shadow';
					}
				}
				return '';
			})()
		`, &chatLocation))

		if chatLocation != "" {
			chatFound = true
			fmt.Printf("  Chat encontrado em: %s\n", chatLocation)
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
		return nil, fmt.Errorf("chat não abriu - input não encontrado")
	}

	// Helper JS function that queries inside the correct context (iframe/shadow/main)
	// We'll inject this as a function in the page
	queryFnSetup := fmt.Sprintf(`
		window.__chatCtx = '%s';
		window.__getChatDoc = function() {
			if (window.__chatCtx.startsWith('iframe')) {
				var idx = parseInt(window.__chatCtx.split('-')[1]);
				var iframe = document.querySelectorAll('iframe')[idx];
				if (iframe) return iframe.contentDocument || iframe.contentWindow.document;
			}
			if (window.__chatCtx === 'shadow') {
				var all = document.querySelectorAll('*');
				for (var i = 0; i < all.length; i++) {
					if (all[i].shadowRoot && all[i].shadowRoot.querySelector('#chat-input'))
						return all[i].shadowRoot;
				}
			}
			return document;
		};
	`, chatLocation)
	chromedp.Run(ctx, chromedp.Evaluate(queryFnSetup, nil))

	// Step 4: Start a new conversation
	fmt.Println("  Iniciando nova conversa...")
	chromedp.Run(ctx, chromedp.Evaluate(`
		(function() {
			var doc = window.__getChatDoc();
			if (!doc) return false;
			var btns = doc.querySelectorAll('button');
			for (var i = 0; i < btns.length; i++) {
				var label = btns[i].getAttribute('aria-label') || '';
				if (label.includes('nova') || label.includes('new') || label.includes('Nova conversa')) {
					btns[i].click();
					return true;
				}
			}
			return false;
		})()
	`, nil))
	time.Sleep(2 * time.Second)

	// Step 5: Send the initial prompt
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

	// Step 6: Send the numbers
	numbersText := strings.Join(numbers, "\n")
	fmt.Printf("  Enviando %d números...\n", len(numbers))
	countBeforeNumbers := getAssistantMsgCount(ctx)
	if err := sendChatMessage(ctx, numbersText); err != nil {
		return nil, fmt.Errorf("enviar números: %w", err)
	}

	// Step 7: Wait for the analysis response
	fmt.Println("  Aguardando análise do MELI (pode levar até 60 segundos)...")
	if err := waitForResponse(ctx, countBeforeNumbers, 90*time.Second); err != nil {
		return nil, fmt.Errorf("aguardar análise: %w", err)
	}

	// Step 8: Extract the response
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

	// Step 9: Parse the response
	results := parseResponse(responseText, numbers)
	return results, nil
}

// sendChatMessage types a message into the chat input and sends it.
// Uses the iframe's own window for React value setters and events.
func sendChatMessage(ctx context.Context, message string) error {
	var result string
	if err := chromedp.Run(ctx, chromedp.Evaluate(fmt.Sprintf(`
		(function() {
			// Resolve the iframe window and document
			var iframeWin, iframeDoc;
			if (window.__chatCtx && window.__chatCtx.startsWith('iframe')) {
				var idx = parseInt(window.__chatCtx.split('-')[1]);
				var iframe = document.querySelectorAll('iframe')[idx];
				if (!iframe) return 'no-iframe';
				iframeWin = iframe.contentWindow;
				iframeDoc = iframe.contentDocument || iframeWin.document;
			} else {
				iframeWin = window;
				iframeDoc = document;
			}
			if (!iframeDoc) return 'no-doc';

			var input = iframeDoc.querySelector('#chat-input') ||
						iframeDoc.querySelector('textarea[placeholder*="Pergunte"]') ||
						iframeDoc.querySelector('textarea[aria-label*="chat"]');
			if (!input) return 'no-input';

			input.focus();
			// Use the IFRAME's own prototype setter (critical for React)
			var setter = Object.getOwnPropertyDescriptor(iframeWin.HTMLTextAreaElement.prototype, 'value').set;
			setter.call(input, %q);
			// Dispatch events using the iframe's own Event constructor
			input.dispatchEvent(new iframeWin.Event('input', { bubbles: true }));
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
	chromedp.Run(ctx, chromedp.Evaluate(`
		(function() {
			var iframeWin, iframeDoc;
			if (window.__chatCtx && window.__chatCtx.startsWith('iframe')) {
				var idx = parseInt(window.__chatCtx.split('-')[1]);
				var iframe = document.querySelectorAll('iframe')[idx];
				if (!iframe) return 'no-iframe';
				iframeWin = iframe.contentWindow;
				iframeDoc = iframe.contentDocument || iframeWin.document;
			} else {
				iframeWin = window;
				iframeDoc = document;
			}
			if (!iframeDoc) return 'no-doc';

			// Look for send button
			var btns = iframeDoc.querySelectorAll('button');
			for (var i = 0; i < btns.length; i++) {
				var label = btns[i].getAttribute('aria-label') || '';
				if (label.includes('nviar') || label.includes('end') || label.includes('Enviar')) {
					btns[i].click();
					return 'sent-button';
				}
			}
			// Fallback: Enter key on the input
			var input = iframeDoc.querySelector('#chat-input') || iframeDoc.querySelector('textarea[placeholder*="Pergunte"]');
			if (input) {
				input.dispatchEvent(new iframeWin.KeyboardEvent('keydown', {key: 'Enter', code: 'Enter', keyCode: 13, which: 13, bubbles: true}));
				return 'sent-enter';
			}
			return 'not-found';
		})()
	`, nil))

	time.Sleep(1 * time.Second)
	return nil
}

// getAssistantMsgCount returns the current count of assistant messages in the chat.
func getAssistantMsgCount(ctx context.Context) int {
	var count int
	chromedp.Run(ctx, chromedp.Evaluate(`
		(function() {
			var doc = window.__getChatDoc();
			if (!doc) return 0;
			return doc.querySelectorAll('.message-item--assistant').length;
		})()
	`, &count))
	return count
}

// waitForResponse waits until a NEW assistant message appears and stabilizes.
// initialCount is the number of assistant messages before the user message was sent.
func waitForResponse(ctx context.Context, initialCount int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	time.Sleep(2 * time.Second)

	prevText := ""
	stableCount := 0

	for time.Now().Before(deadline) {
		currentCount := getAssistantMsgCount(ctx)

		if currentCount > initialCount {
			// New message appeared — read its text
			var currentText string
			chromedp.Run(ctx, chromedp.Evaluate(`
				(function() {
					var doc = window.__getChatDoc();
					if (!doc) return '';
					var msgs = doc.querySelectorAll('.message-item--assistant');
					if (msgs.length === 0) return '';
					var last = msgs[msgs.length - 1];
					var content = last.querySelector('.chat-message__content');
					return content ? content.textContent.trim() : last.textContent.trim();
				})()
			`, &currentText))

			if currentText != "" && currentText == prevText {
				stableCount++
				if stableCount >= 3 {
					return nil // text hasn't changed in ~6s
				}
			} else {
				stableCount = 0
				prevText = currentText
			}
		}

		time.Sleep(2 * time.Second)
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
	err := chromedp.Run(ctx, chromedp.Evaluate(`
		(function() {
			var doc = window.__getChatDoc();
			if (!doc) return '';
			var msgs = doc.querySelectorAll('.message-item--assistant');
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
	re := regexp.MustCompile(`\d{7,13}`)
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
