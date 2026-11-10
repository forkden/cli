-- Three tables, nine rows: the forkden review quickstart fixture.
CREATE TABLE customers (id integer PRIMARY KEY, email text NOT NULL, country text);
CREATE TABLE orders (id bigint PRIMARY KEY, customer_id integer NOT NULL REFERENCES customers (id), status text NOT NULL, total numeric(10, 2) NOT NULL);
CREATE TABLE order_items (order_id bigint REFERENCES orders (id), line integer, sku text NOT NULL, qty integer NOT NULL, PRIMARY KEY (order_id, line));
INSERT INTO customers VALUES (1, 'ada@example.com', 'GB'), (2, 'linus@example.com', NULL), (3, 'grace@example.com', NULL);
INSERT INTO orders VALUES (1, 1, 'paid', 40.00), (2, 2, 'pending', 12.50), (3, 3, 'pending', 7.25);
INSERT INTO order_items VALUES (1, 1, 'BOOK-1', 2), (2, 1, 'PEN-3', 5), (3, 1, 'MUG-2', 1);
