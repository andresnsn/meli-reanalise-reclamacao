# meli-reanalise-reclamacao

Aplicação em Go para solicitar a reanálise de reclamações no Mercado Livre via chat do assistente.

## Funcionalidade

- Envia reclamações/vendas em lotes de até 50 para o chat do assistente do MELI
- Aguarda o retorno de cada lote antes de enviar o próximo
- Gera planilha Excel com o resultado consolidado

## Compilar

```bash
# Linux/Mac
go build -o meli-reanalise-reclamacao .

# Windows 64 bits / Intel-AMD (recomendado para a maioria dos PCs)
GOOS=windows GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o ReanaliseReclamacaoML.exe .

# Windows ARM64 (Copilot+ PC / Surface com Snapdragon)
GOOS=windows GOARCH=arm64 go build -trimpath -ldflags="-s -w" -o ReanaliseReclamacaoML-arm64.exe .

# Windows 32 bits (fallback para máquinas antigas)
GOOS=windows GOARCH=386 go build -trimpath -ldflags="-s -w" -o ReanaliseReclamacaoML-32bits.exe .
```

### Metadados do executável Windows (publisher/versão + manifest)

O build para Windows embute automaticamente metadados de versão/fabricante e um
manifest de compatibilidade (`versioninfo.json` + `app.manifest`). Isso ajuda a
reduzir avisos de *reputação* do SmartScreen/Defender ("editor desconhecido"),
mas **não substitui uma assinatura digital**: o binário continua SEM assinatura
Authenticode. Os arquivos `resource_windows_*.syso` (amd64, arm64 e 386) são
gerados a partir do `versioninfo.json`. Para regerá-los após editar os metadados:

```bash
go install github.com/josephspurrier/goversioninfo/cmd/goversioninfo@latest
go generate ./...
```

> Não deixe um arquivo `resource.syso` sem sufixo de arquitetura no diretório —
> ele quebra o build 32 bits (`unknown relocation type 3`). Use apenas os nomes
> `resource_windows_amd64.syso`, `resource_windows_arm64.syso` e
> `resource_windows_386.syso`.

### Erro "Esse aplicativo não pode ser executado no seu PC"

Esse aviso do Windows tem duas causas comuns, tratadas de formas diferentes:

1. **Arquitetura incompatível** (causa mais frequente): o `.exe` foi compilado
   para uma CPU diferente da máquina. Rode a variante que corresponde ao seu
   Windows — `amd64` (Intel/AMD, maioria dos PCs), `arm64` (Copilot+ PC /
   Snapdragon) ou `386` (32 bits, máquinas antigas). Metadados/manifest **não**
   corrigem esse caso; só o build da arquitetura certa.
2. **Política corporativa / reputação** (AppLocker, WDAC, SmartScreen): o
   ambiente bloqueia executáveis de editor desconhecido/sem assinatura. Os
   metadados e o manifest ajudam, mas o fim definitivo do aviso exige um
   **certificado de code-signing (Authenticode/EV)** aplicado ao `.exe`.

## Usar

1. Execute o programa
2. Faça login no Mercado Livre na janela do Chrome que abrir (primeira execução)
3. Cole os números de venda ou reclamação no terminal
4. Aguarde o processamento e a geração da planilha

## Colunas da planilha

| Coluna | Descrição |
|--------|-----------|
| Número da operação | Número da venda/reclamação |
| Resultado | Excluída / Continua impactando / Sem impacto na reputação |

## Resumo

A planilha inclui um resumo com totais: analisados, excluídos, continuam impactando, sem impacto.
