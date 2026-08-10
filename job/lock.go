package job

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// Locker — распределённая блокировка поверх Redis.
//
// # ЗАЧЕМ ОНА НУЖНА
//
// scheduler можно запустить в нескольких репликах (для отказоустойчивости:
// узел с единственной репликой может уехать в обслуживание). Но рассылка
// вопросов не должна выполниться дважды — пользователь получит два
// одинаковых сообщения.
//
// Вариант «replicas: 1 и не выдумывать» тоже рабочий и часто правильный.
// Его минус: при падении узла рассылка просто не произойдёт, и никто
// об этом не узнает до жалобы пользователя.
//
// Блокировка во внешнем хранилище — это ещё и Фактор 6 (Processes):
// координация между процессами живёт НЕ в памяти процесса, а в backing
// service. Именно поэтому процессы остаются взаимозаменяемыми.
//
// # ЧЕСТНАЯ ОГОВОРКА ПРО НАДЁЖНОСТЬ
//
// Это не Redlock и не строгая взаимная блокировка. Одиночный Redis —
// единая точка отказа, а при его failover'е блокировку теоретически
// могут получить двое. Для «не отправить вопрос дважды» такой гарантии
// достаточно: цена ошибки — лишнее сообщение. Для списания денег
// так делать нельзя, там нужна транзакционная идемпотентность в самой
// целевой системе.
type Locker struct {
	rdb *redis.Client
	ttl time.Duration
}

func NewLocker(rdb *redis.Client, ttl time.Duration) *Locker {
	return &Locker{rdb: rdb, ttl: ttl}
}

// Acquire пытается взять блокировку. Возвращает функцию освобождения.
//
// SET key value NX EX ttl — атомарная операция «поставь, если ключа нет».
// Именно атомарность здесь ключевая: связка EXISTS + SET была бы гонкой,
// две реплики успели бы пройти проверку одновременно.
//
// TTL обязателен. Без него упавший процесс оставит блокировку навсегда,
// и рассылка не произойдёт больше никогда — отказ, который заметят
// далеко не сразу.
func (l *Locker) Acquire(ctx context.Context, key string) (release func(), ok bool, err error) {
	// Уникальное значение владельца: снимать блокировку имеет право
	// только тот, кто её поставил. Иначе процесс, у которого блокировка
	// уже истекла по TTL, снял бы чужую.
	token := make([]byte, 16)
	if _, err := rand.Read(token); err != nil {
		return nil, false, err
	}
	owner := hex.EncodeToString(token)

	acquired, err := l.rdb.SetNX(ctx, key, owner, l.ttl).Result()
	if err != nil {
		return nil, false, fmt.Errorf("redis setnx: %w", err)
	}
	if !acquired {
		return nil, false, nil
	}

	return func() {
		// Сравнить значение и удалить — одной Lua-скриптом, атомарно.
		// Разнести на GET и DEL нельзя: между ними блокировка может
		// истечь, её возьмёт другой процесс, и мы удалим уже чужую.
		const script = `
			if redis.call("get", KEYS[1]) == ARGV[1] then
				return redis.call("del", KEYS[1])
			else
				return 0
			end`
		releaseCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = redis.NewScript(script).Run(releaseCtx, l.rdb, []string{key}, owner).Err()
	}, true, nil
}

func (l *Locker) Ping(ctx context.Context) error { return l.rdb.Ping(ctx).Err() }
