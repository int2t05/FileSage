// 本文件将 CSV/TSV 文本解析为有界表格数据。
export type TableDelimiter = ',' | '\t';

// 限制展开后的表格规模，避免小文件生成过多单元格。
const MAX_TABLE_COLUMNS = 256;
const MAX_TABLE_CELLS = 50_000;

// 按引号规则拆分字段，不解释单元格内容。
export function parseDelimitedText(source: string, delimiter: TableDelimiter): string[][] {
  if (delimiter !== ',' && delimiter !== '\t') {
    throw new Error('仅支持 CSV 和 TSV 分隔符。');
  }

  const text = source.startsWith('\uFEFF') ? source.slice(1) : source;
  const records: string[][] = [];
  let record: string[] = [];
  let field = '';
  let inQuotes = false;
  let afterQuote = false;
  let recordStarted = false;
  let line = 1;
  let cellCount = 0;
  const format = delimiter === ',' ? 'CSV' : 'TSV';

  const error = (message: string): Error => new Error(`${format} 第${line}行第${record.length + 1}列：${message}`);
  const finishField = () => {
    if (record.length >= MAX_TABLE_COLUMNS) throw error(`列数超过 ${MAX_TABLE_COLUMNS} 列的阅读上限。`);
    if (cellCount >= MAX_TABLE_CELLS) throw error(`单元格超过 ${MAX_TABLE_CELLS.toLocaleString('zh-CN')} 个的阅读上限。`);
    record.push(field);
    cellCount += 1;
    field = '';
    afterQuote = false;
  };
  const finishRecord = () => {
    finishField();
    records.push(record);
    record = [];
    recordStarted = false;
  };

  for (let index = 0; index < text.length; index += 1) {
    const character = text[index];
    const isNewline = character === '\r' || character === '\n';
    const newlineWidth = character === '\r' && text[index + 1] === '\n' ? 2 : 1;

    if (inQuotes) {
      if (character === '"') {
        if (text[index + 1] === '"') {
          field += '"';
          index += 1;
        } else {
          inQuotes = false;
          afterQuote = true;
        }
      } else {
        field += character;
        if (isNewline) {
          if (newlineWidth === 2) {
            field += '\n';
            index += 1;
          }
          line += 1;
        }
      }
      continue;
    }

    if (afterQuote) {
      if (character === delimiter) {
        finishField();
        recordStarted = true;
      } else if (isNewline) {
        finishRecord();
        if (newlineWidth === 2) index += 1;
        line += 1;
      } else {
        throw error('闭合引号后出现多余字符。');
      }
      continue;
    }

    if (character === '"') {
      if (field.length !== 0) throw error('未引用字段中出现引号。');
      inQuotes = true;
      recordStarted = true;
    } else if (character === delimiter) {
      finishField();
      recordStarted = true;
    } else if (isNewline) {
      finishRecord();
      if (newlineWidth === 2) index += 1;
      line += 1;
    } else {
      field += character;
      recordStarted = true;
    }
  }

  if (inQuotes) throw error('字段中的引号未闭合。');
  if (recordStarted || field.length > 0 || record.length > 0 || afterQuote) {
    finishField();
    records.push(record);
  }

  return records;
}
