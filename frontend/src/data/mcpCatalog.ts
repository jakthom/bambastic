import catalog from '../../../internal/bridge/catalog.json';

export interface McpOperation {
  name: string;
  category: string;
  description: string;
  readOnly: boolean;
  inputSchema: {
    properties: Readonly<Record<string, unknown>>;
    required: readonly string[];
  };
}

// Go embeds this same catalog for list_categories, search_tools and execute_tool.
export const mcpOperations: readonly McpOperation[] = catalog.map(({ name, category, description, method, input_schema }) => ({
  name,
  category,
  description,
  readOnly: method === 'GET',
  inputSchema: { properties: input_schema.properties, required: input_schema.required ?? [] },
}));

export const mcpCategories = Array.from(new Set(mcpOperations.map(operation => operation.category))).sort();
