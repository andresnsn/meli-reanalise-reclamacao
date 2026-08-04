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

# Windows 64 bits (recomendado)
GOOS=windows GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o ReanaliseReclamacaoML.exe .

# Windows 32 bits (fallback para máquinas antigas)
GOOS=windows GOARCH=386 go build -trimpath -ldflags="-s -w" -o ReanaliseReclamacaoML-32bits.exe .
```

### Metadados do executável Windows (publisher/versão + manifest)

O build para Windows embute automaticamente metadados de versão/fabricante e um
manifest de compatibilidade (`versioninfo.json` + `app.manifest`), o que reduz
falsos positivos de SmartScreen/Defender ("Esse aplicativo não pode ser executado
no seu PC"). Os arquivos `resource_windows_*.syso` são gerados a partir do
`versioninfo.json`. Para regerá-los após editar os metadados:

```bash
go install github.com/josephspurrier/goversioninfo/cmd/goversioninfo@latest
go generate ./...
```

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
