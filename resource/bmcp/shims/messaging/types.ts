export type MessageMap = Record<string, { payload: unknown; result: unknown }>;

export type MessageType<M extends MessageMap> = keyof M & string;

export type MessagePayload<
  M extends MessageMap,
  T extends MessageType<M>,
> = M[T]["payload"];

export type MessageResult<
  M extends MessageMap,
  T extends MessageType<M>,
> = M[T]["result"];
